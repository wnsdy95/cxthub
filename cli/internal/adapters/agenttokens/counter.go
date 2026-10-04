// Package agenttokens counts CXTHub-owned plain text locally. It never queries
// a provider, opens credentials, or infers a model's context window.
package agenttokens

import (
	"container/list"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/tiktoken-go/tokenizer"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

const (
	cacheEntries = 128
	maxTextBytes = 16 << 20
	// The underlying BPE merge is quadratic in an unbroken regex piece. Refuse
	// pathological pieces before entering it; do not split and miscount BPE seams.
	maxRunBytes = 4096
	// Bound aggregate worst-case merging work as well as each individual run.
	// Otherwise thousands of individually permitted long pieces could stall a
	// cancelled preparation. Ordinary 800k-token prose fits this work allowance.
	maxMergeWork = 128 << 20
)

type cacheKey struct {
	encoding tokenizer.Encoding
	hash     [32]byte
}
type cacheEntry struct {
	key   cacheKey
	usage domain.AgentTokenUsage
}

// Counter retains only hashes and counts, never prompt text. One bounded
// computation per instance avoids concurrent large allocations. Waiting callers
// honor cancellation. Construct with New; do not copy an initialized Counter.
type Counter struct {
	gate   chan struct{}
	codecs map[tokenizer.Encoding]tokenizer.Codec
	splits map[tokenizer.Encoding]*regexp2.Regexp
	cache  map[cacheKey]*list.Element
	lru    *list.List
}

func New() *Counter {
	return &Counter{gate: make(chan struct{}, 1), codecs: make(map[tokenizer.Encoding]tokenizer.Codec), splits: make(map[tokenizer.Encoding]*regexp2.Regexp), cache: make(map[cacheKey]*list.Element), lru: list.New()}
}

var _ outbound.AgentTokenCounter = (*Counter)(nil)

func (c *Counter) CountAgentTokens(ctx context.Context, provider domain.ProviderKind, model, text string) (domain.AgentTokenUsage, error) {
	if err := ctx.Err(); err != nil {
		return domain.AgentTokenUsage{}, err
	}
	if provider != domain.ProviderCodex && provider != domain.ProviderClaude {
		return domain.AgentTokenUsage{}, domain.ErrUnsupportedProvider
	}
	if !utf8.ValidString(text) {
		return domain.AgentTokenUsage{}, fmt.Errorf("%w: token input is not valid UTF-8", domain.ErrHashMismatch)
	}
	encoding := encodingFor(provider, model)
	if encoding == "" {
		return byteAllowance(text, "model_tokenizer_unavailable"), nil
	}
	if len(text) > maxTextBytes {
		return byteAllowance(text, "tokenizer_work_limit"), nil
	}
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return domain.AgentTokenUsage{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return domain.AgentTokenUsage{}, err
	}
	key := cacheKey{encoding: encoding, hash: sha256.Sum256([]byte(text))}
	if entry := c.cache[key]; entry != nil {
		c.lru.MoveToFront(entry)
		return entry.Value.(cacheEntry).usage, nil
	}
	reason, err := c.inputFallbackReason(ctx, encoding, text)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return domain.AgentTokenUsage{}, ctxErr
	}
	if err != nil {
		return domain.AgentTokenUsage{}, err
	} else if reason != "" {
		return byteAllowance(text, reason), nil
	}
	codec := c.codecs[encoding]
	if codec == nil {
		var err error
		codec, err = tokenizer.Get(encoding)
		if err != nil {
			return domain.AgentTokenUsage{}, fmt.Errorf("%w: local tokenizer unavailable", domain.ErrProviderCapabilityUnknown)
		}
		c.codecs[encoding] = codec
	}
	if err := ctx.Err(); err != nil {
		return domain.AgentTokenUsage{}, err
	}
	// Count treats special-token spellings as ordinary text. User-controlled
	// strings such as <|endoftext|> must never be assigned hidden control IDs.
	n, err := codec.Count(text)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return domain.AgentTokenUsage{}, ctxErr
	}
	if err != nil {
		// Do not propagate library errors that might include private input.
		return domain.AgentTokenUsage{}, fmt.Errorf("%w: local tokenizer failed", domain.ErrProviderCapabilityUnknown)
	}
	usage := domain.AgentTokenUsage{Tokens: n, Exact: true, Tokenizer: "tiktoken-go/" + string(encoding) + "@0.8.1/ordinary-v1/ucd-" + unicode.Version, Scope: "text"}
	c.cache[key] = c.lru.PushFront(cacheEntry{key: key, usage: usage})
	if c.lru.Len() > cacheEntries {
		old := c.lru.Back()
		delete(c.cache, old.Value.(cacheEntry).key)
		c.lru.Remove(old)
	}
	return usage, nil
}

func byteAllowance(text, reason string) domain.AgentTokenUsage {
	return domain.AgentTokenUsage{Tokens: len(text), Exact: false, Tokenizer: domain.UTF8ByteBoundCounter, Scope: "text", Reason: reason}
}

// Mapping follows OpenAI tiktoken/model.py at 4e71bbe0c078468e00fefbf94b39849389f346e5.
// Limit it to relevant text models. This is an encoding mapping, not proof of
// model existence, provider routing, available capacity or host input. Never
// guess a future family, deployment alias or Claude encoding from its name.
func encodingFor(provider domain.ProviderKind, model string) tokenizer.Encoding {
	if provider != domain.ProviderCodex {
		return ""
	}
	for _, family := range []string{"gpt-4o", "gpt-4.1", "o1", "o3", "o4-mini"} {
		if model == family || strings.HasPrefix(model, family+"-") {
			return tokenizer.O200kBase
		}
	}
	if strings.HasPrefix(model, "gpt-4.5-") || strings.HasPrefix(model, "chatgpt-4o-") {
		return tokenizer.O200kBase
	}
	if model == "gpt-5" || strings.HasPrefix(model, "gpt-5-") || (strings.HasPrefix(model, "gpt-5.") && len(model) > 6 && model[6] >= '0' && model[6] <= '9') {
		return tokenizer.O200kBase
	}
	for _, family := range []string{"gpt-4", "gpt-3.5-turbo", "gpt-35-turbo"} {
		if model == family || strings.HasPrefix(model, family+"-") {
			return tokenizer.Cl100kBase
		}
	}
	return ""
}

func compatibleUnicodeRune(r rune) bool {
	// regexp2 classifies with Go's UCD. Unassigned/newer characters must not
	// be treated as punctuation while the reference tokenizer treats them as
	// letters (e.g. U+A7CB). Revisit these gates when upgrading either table.
	// Existing assignments with category/simple-fold changes between UCD 15
	// and the reference's UCD 16 are also excluded conservatively. Derived
	// from unicode.org/Public/{15.0.0,16.0.0}/ucd/{UnicodeData,CaseFolding}.txt.
	switch r {
	case '\U0001171e', '\ufb05', '\u1fe3', '\u1fd3':
		return false
	}
	return unicode.Version == "15.0.0" && unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs)
}
