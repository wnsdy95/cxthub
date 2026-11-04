package domain

import (
	"context"
	"errors"
	"io"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const DocDisplayStepSourceBytes = 64 << 10
const docDisplayEncodedRunBytes = 128
const docDisplayEncodedHeaderBytes = 512

func BuildDocDisplayStringSourceReader(ctx context.Context, eventHash ContentHash, eventBytes int, reader io.Reader) (source DocDisplayStringSource, failure error) {
	defer func() {
		if errors.Is(failure, io.EOF) || errors.Is(failure, io.ErrUnexpectedEOF) {
			source, failure = DocDisplayStringSource{}, ErrIntegrity
		}
		if err := ctx.Err(); err != nil {
			source, failure = DocDisplayStringSource{}, err
		}
	}()
	if eventBytes > MaxConversationDocumentBytes {
		return DocDisplayStringSource{}, ErrIntegrity
	}
	state, err := discoverDocFieldReader(ctx, eventHash, eventBytes, reader)
	if err != nil {
		return DocDisplayStringSource{}, err
	}
	return state.DisplaySource()
}

type DocDisplayStringStep struct {
	State     DocDisplayStringState
	Page      *DocDisplayPage
	Objects   []DocRangeObject
	ReadBytes int
}

type docDisplayStringInput struct {
	ctx      context.Context
	load     func(context.Context, int, int) ([]byte, error)
	end      int
	base     int
	readTo   int
	used     int
	maxRead  int
	position int
	raw      []byte
}

func (input *docDisplayStringInput) offset() int { return input.base + input.position }

func (input *docDisplayStringInput) ready(target int) (bool, error) {
	if err := input.ctx.Err(); err != nil {
		return false, err
	}
	remaining := len(input.raw) - input.position
	if remaining >= 12 || input.readTo == input.end {
		return remaining > 0, nil
	}
	maxRead := input.maxRead
	if maxRead == 0 {
		maxRead = DocDisplayStepSourceBytes
	}
	length := min(4096, max(12, target), maxRead-input.used, input.end-input.readTo)
	if input.used == 0 {
		length = min(length, 1024)
	}
	if length <= 0 {
		return input.bufferedRuneReady(), nil
	}
	raw, err := input.load(input.ctx, input.readTo, length)
	if err != nil {
		return false, err
	}
	if len(raw) != length {
		return false, ErrIntegrity
	}
	input.base += input.position
	if input.raw == nil {
		input.raw = make([]byte, 0, 4096+12)
	}
	copy(input.raw, input.raw[input.position:])
	input.raw = append(input.raw[:remaining], raw...)
	input.position = 0
	input.readTo += length
	input.used += length
	return len(input.raw) >= 12 || input.readTo == input.end || input.bufferedRuneReady(), nil
}

func (input *docDisplayStringInput) bufferedRuneReady() bool {
	if input.maxRead == 0 || input.used < input.maxRead || input.position == len(input.raw) {
		return false
	}
	raw := input.raw[input.position:]
	if raw[0] != '\\' {
		return utf8.FullRune(raw)
	}
	if len(raw) < 2 {
		return false
	}
	if raw[1] != 'u' {
		return true
	}
	if len(raw) < 6 {
		return false
	}
	unit, err := docDisplayHexRune(raw)
	if err != nil || unit < 0xd800 || unit > 0xdbff {
		return true
	}
	return len(raw) >= 12 || len(raw) > 6 && raw[6] != '\\' || len(raw) > 7 && raw[7] != 'u'
}

func StepDocDisplayString(ctx context.Context, source DocDisplayStringSource, state DocDisplayStringState, load func(context.Context, int, int) ([]byte, error)) (result DocDisplayStringStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocDisplayStringStep{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocDisplayStringStep{}, err
	}
	if state.Validate(source) != nil || state.Done || load == nil {
		return DocDisplayStringStep{}, ErrIntegrity
	}
	decoded, err := decodeDocJSONStringPage(ctx, docJSONStringPageRequest{Version: 1, Source: state.Source, Token: source.Token, Next: state.Next, Index: state.Directory.Count, From: state.TextBytes}, load)
	if err != nil {
		return DocDisplayStringStep{}, err
	}
	next := state
	next.Next, next.Done, next.HasContent = decoded.Next, decoded.Done, state.HasContent || decoded.HasContent
	result.ReadBytes, result.Page = decoded.ReadBytes, decoded.Page
	if decoded.Page != nil {
		encoded, err := decoded.Page.Encode(source)
		if err != nil {
			return DocDisplayStringStep{}, err
		}
		next.Directory, result.Objects, err = state.Directory.Append(ctx, HashContent(encoded))
		if err != nil {
			return DocDisplayStringStep{}, err
		}
		result.Objects = append(result.Objects, DocRangeObject{Hash: HashContent(encoded), Bytes: encoded})
		next.TextBytes += len(decoded.Page.Text)
	}
	if next.Validate(source) != nil {
		return DocDisplayStringStep{}, ErrIntegrity
	}
	result.State = next
	return result, nil
}

type docJSONStringPageRequest struct {
	Version  int
	Source   ContentHash
	Token    DocFindSpan
	Next     int
	Index    int
	From     int
	MaxBytes int
	MaxRuns  int
	MaxRead  int
}

type docJSONStringPageResult struct {
	Next       int
	Done       bool
	HasContent bool
	Page       *DocDisplayPage
	ReadBytes  int
}

func decodeDocJSONStringPage(ctx context.Context, request docJSONStringPageRequest, load func(context.Context, int, int) ([]byte, error)) (docJSONStringPageResult, error) {
	if request.MaxBytes < 0 || request.MaxBytes > DocDisplayPageTextBytes || request.MaxRuns < 0 || request.MaxRuns > DocDisplayPageRuns || request.MaxRead != 0 && (request.MaxRead < 12 || request.MaxRead > DocDisplayStepSourceBytes) {
		return docJSONStringPageResult{}, ErrIntegrity
	}
	maxBytes, maxRuns := request.MaxBytes, request.MaxRuns
	if maxBytes == 0 {
		maxBytes = DocDisplayPageTextBytes
	}
	if maxRuns == 0 {
		maxRuns = DocDisplayPageRuns
	}
	input := docDisplayStringInput{ctx: ctx, load: load, end: request.Token.To, base: request.Next, readTo: request.Next, maxRead: request.MaxRead}
	page := DocDisplayPage{Version: request.Version, Source: request.Source, Index: request.Index, From: request.From}
	var result docJSONStringPageResult
	var text strings.Builder
	encodedTextBytes := 0
	if request.Next == request.Token.From {
		available, err := input.ready(maxBytes)
		if err != nil {
			return docJSONStringPageResult{}, err
		}
		if !available || input.raw[0] != '"' {
			return docJSONStringPageResult{}, ErrIntegrity
		}
		input.position++
	}
	for text.Len() < maxBytes && len(page.Runs) < maxRuns {
		if err := ctx.Err(); err != nil {
			return docJSONStringPageResult{}, err
		}
		available, err := input.ready(min(maxBytes-text.Len(), (maxRuns-len(page.Runs))*12))
		if err != nil {
			return docJSONStringPageResult{}, err
		}
		if !available {
			break
		}
		raw := input.raw[input.position:]
		if raw[0] == '"' {
			if input.offset() != request.Token.To-1 {
				return docJSONStringPageResult{}, ErrIntegrity
			}
			input.position++
			result.Done = true
			break
		}
		character, width, linear, err := decodeDocDisplayStringRune(raw)
		if err != nil {
			return docJSONStringPageResult{}, err
		}
		if text.Len()+utf8.RuneLen(character) > maxBytes || len(page.Runs) == maxRuns {
			break
		}
		runs := len(page.Runs)
		if !linear || runs == 0 || !page.Runs[runs-1].Linear {
			runs++
		}
		encodedRuneBytes := docDisplayEncodedRuneBytes(character)
		if encodedTextBytes+encodedRuneBytes+docDisplayEncodedHeaderBytes+runs*docDisplayEncodedRunBytes > MaxDocDisplayPageBytes {
			break
		}
		from, to := input.offset(), input.offset()+width
		if linear {
			_, _ = text.Write(raw[:width])
		} else {
			_, _ = text.WriteRune(character)
		}
		if linear && len(page.Runs) > 0 && page.Runs[len(page.Runs)-1].Linear {
			last := &page.Runs[len(page.Runs)-1]
			if last.SourceTo != from {
				return docJSONStringPageResult{}, ErrIntegrity
			}
			last.SourceTo, last.To = to, text.Len()
		} else {
			page.Runs = append(page.Runs, DocDisplayRun{To: text.Len(), SourceFrom: from, SourceTo: to, Linear: linear})
		}
		if !docMarkdownWhitespace(character) {
			result.HasContent = true
		}
		encodedTextBytes += encodedRuneBytes
		input.position += width
	}
	result.Next = input.offset()
	if result.Next <= request.Next || text.Len() == 0 && !result.Done {
		return docJSONStringPageResult{}, ErrIntegrity
	}
	result.ReadBytes = input.used
	if text.Len() > 0 {
		page.Text = text.String()
		result.Page = &page
	}
	return result, nil
}

func docDisplayEncodedRuneBytes(character rune) int {
	switch character {
	case '"', '\\', '\b', '\f', '\n', '\r', '\t':
		return 2
	case '<', '>', '&', '\u2028', '\u2029':
		return 6
	default:
		if character < 0x20 {
			return 6
		}
		return utf8.RuneLen(character)
	}
}

func decodeDocDisplayStringRune(raw []byte) (rune, int, bool, error) {
	if len(raw) == 0 {
		return 0, 0, false, ErrIntegrity
	}
	if raw[0] != '\\' {
		if raw[0] < 0x20 || raw[0] == '"' {
			return 0, 0, false, ErrIntegrity
		}
		character, width := utf8.DecodeRune(raw)
		if character == utf8.RuneError && width == 1 {
			return 0, 0, false, ErrIntegrity
		}
		return character, width, true, nil
	}
	if len(raw) < 2 {
		return 0, 0, false, ErrIntegrity
	}
	switch raw[1] {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		return docDisplaySimpleEscape(raw[1]), 2, false, nil
	case 'u':
		unit, err := docDisplayHexRune(raw)
		if err != nil {
			return 0, 0, false, err
		}
		if unit >= 0xd800 && unit <= 0xdbff && len(raw) >= 12 && raw[6] == '\\' && raw[7] == 'u' {
			low, err := docDisplayHexRune(raw[6:])
			if err != nil {
				return 0, 0, false, err
			}
			if low >= 0xdc00 && low <= 0xdfff {
				return utf16.DecodeRune(unit, low), 12, false, nil
			}
		}
		if utf16.IsSurrogate(unit) {
			return utf8.RuneError, 6, false, nil
		}
		return unit, 6, false, nil
	default:
		return 0, 0, false, ErrIntegrity
	}
}

func docDisplayHexRune(raw []byte) (rune, error) {
	if len(raw) < 6 {
		return 0, ErrIntegrity
	}
	var result rune
	for _, digit := range raw[2:6] {
		value, ok := docDisplayHexDigit(digit)
		if !ok {
			return 0, ErrIntegrity
		}
		result = result*16 + value
	}
	return result, nil
}

func docDisplayHexDigit(digit byte) (rune, bool) {
	switch {
	case digit >= '0' && digit <= '9':
		return rune(digit - '0'), true
	case digit >= 'a' && digit <= 'f':
		return rune(digit - 'a' + 10), true
	case digit >= 'A' && digit <= 'F':
		return rune(digit - 'A' + 10), true
	default:
		return 0, false
	}
}

func docDisplaySimpleEscape(escape byte) rune {
	switch escape {
	case 'b':
		return '\b'
	case 'f':
		return '\f'
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	default:
		return rune(escape)
	}
}
