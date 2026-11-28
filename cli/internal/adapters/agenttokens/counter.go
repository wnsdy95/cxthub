// Package agenttokens counts CXTHub-owned plain text locally. It never queries
// a provider, opens credentials, or infers a model's context window.
package agenttokens

import (
	"container/list"

	"github.com/dlclark/regexp2/v2"
	"github.com/tiktoken-go/tokenizer"
)

type cacheKey struct {
	encoding tokenizer.Encoding
	hash     [32]byte
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
