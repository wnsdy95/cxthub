package agenttokens

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/tiktoken-go/tokenizer"
)

// These are the ordinary-text boundaries used by tokenizer v0.8.1, from
// codec/{o200k_base,cl100k_base}.go. Keep them pinned with the counter's encoding
// version and independent reference corpus. Do not replace them with whitespace
// or character-category runs: one minified JSON string contains many pieces.
const (
	o200kPattern  = `[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]*[\p{Ll}\p{Lm}\p{Lo}\p{M}]+(?i:'s|'t|'re|'ve|'m|'ll|'d)?|[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]+[\p{Ll}\p{Lm}\p{Lo}\p{M}]*(?i:'s|'t|'re|'ve|'m|'ll|'d)?|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n/]*|\s*[\r\n]+|\s+(?!\S)|\s+`
	cl100kPattern = `(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`
	maxMatchTime  = 250 * time.Millisecond
)

// inputFallbackReason runs under the counter gate. The regex pass bounds the
// actual BPE pieces; counting still receives the original, unsplit text. Context
// cancellation is checked throughout preflight and before/after the bounded
// library Count call. No abandoned background tokenization goroutine is used.
func (c *Counter) inputFallbackReason(ctx context.Context, encoding tokenizer.Encoding, text string) (string, error) {
	nextCheck := 0
	for i, r := range text {
		if i >= nextCheck {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			nextCheck = i + 4096
		}
		if r >= utf8.RuneSelf && !compatibleUnicodeRune(r) {
			return "unicode_classification_unavailable", nil
		}
	}
	re := c.splits[encoding]
	if re == nil {
		pattern := o200kPattern
		if encoding == tokenizer.Cl100kBase {
			pattern = cl100kPattern
		}
		re = regexp2.MustCompile(pattern, regexp2.None)
		re.MatchTimeout = maxMatchTime
		c.splits[encoding] = re
	}
	return checkPieceWork(ctx, re, text)
}

func checkPieceWork(ctx context.Context, re *regexp2.Regexp, text string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var work int64
	consumed, consumedRunes := 0, 0
	match, err := re.FindStringMatch(text)
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if err != nil {
			// Library timeout/errors can contain the input. Only return a fixed
			// reason, never the error or private matched text.
			return "tokenizer_work_limit", nil
		}
		if match == nil {
			break
		}
		if match.RuneIndex != consumedRunes || match.RuneLength == 0 {
			return "tokenizer_work_limit", nil
		}
		// ByteRange lazily allocates an input-sized offset table for Unicode.
		// Walk the match's existing rune slice instead, with an early bound.
		size := 0
		for _, r := range match.Runes() {
			size += utf8.RuneLen(r)
			if size > maxRunBytes {
				return "tokenizer_work_limit", nil
			}
		}
		work += int64(size) * int64(size)
		if work > maxMergeWork {
			return "tokenizer_work_limit", nil
		}
		consumed += size
		consumedRunes += match.RuneLength
		match, err = re.FindNextMatch(match)
	}
	if consumed != len(text) {
		return "tokenizer_work_limit", nil
	}
	return "", nil
}
