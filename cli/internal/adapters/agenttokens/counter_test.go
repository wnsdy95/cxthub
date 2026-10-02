package agenttokens

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/tiktoken-go/tokenizer"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestCounterMatchesIndependentOrdinaryTextReference(t *testing.T) {
	data, err := os.ReadFile("testdata/ordinary.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Encoding, Name, Text string
			Tokens               int
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) < 80 {
		t.Fatal("reference corpus missing")
	}
	c := New()
	for _, item := range fixture.Cases {
		t.Run(item.Encoding+"/"+item.Name, func(t *testing.T) {
			model := "gpt-5.4"
			if item.Encoding == "cl100k_base" {
				model = "gpt-4"
			}
			u, err := c.CountAgentTokens(context.Background(), domain.ProviderCodex, model, item.Text)
			if err != nil || u.Tokens != item.Tokens || !u.Exact || u.Scope != "text" || u.Reason != "" || !strings.Contains(u.Tokenizer, item.Encoding+"@0.8.1/ordinary-v1") {
				t.Fatalf("reference mismatch: got=%+v want=%d err=%v", u, item.Tokens, err)
			}
		})
	}
}

func TestCounterDoesNotGuessProviderOrModel(t *testing.T) {
	c := New()
	for _, provider := range []domain.ProviderKind{domain.ProviderCodex, domain.ProviderClaude} {
		for _, model := range []string{"", "default", "private-deployment", "gpt-6", "gpt-50", "gpt-5.x", "gpt-4.5", "chatgpt-4o", "claude-sonnet-5-5"} {
			u, err := c.CountAgentTokens(context.Background(), provider, model, "\ud55c\uae00 text")
			if err != nil || u.Exact || u.Tokens != len("\ud55c\uae00 text") || u.Tokenizer != domain.UTF8ByteBoundCounter || u.Reason != "model_tokenizer_unavailable" {
				t.Fatalf("%s/%s: %+v %v", provider, model, u, err)
			}
		}
	}
	// A model-looking string on Claude is not an OpenAI tokenizer binding.
	u, err := c.CountAgentTokens(context.Background(), domain.ProviderClaude, "gpt-5", "text")
	if err != nil || u.Exact {
		t.Fatalf("cross-provider mapping: %+v %v", u, err)
	}
	if _, err = c.CountAgentTokens(context.Background(), "unknown", "gpt-5", "text"); !errors.Is(err, domain.ErrUnsupportedProvider) {
		t.Fatal(err)
	}
	if _, err = c.CountAgentTokens(context.Background(), domain.ProviderCodex, "gpt-5", string([]byte{0xff})); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal(err)
	}
}

func TestCounterVersionedModelMapping(t *testing.T) {
	for _, model := range []string{"gpt-5", "gpt-5-mini", "gpt-5.1-codex-mini", "gpt-5.4", "gpt-5.4-2026-03-05", "gpt-4o", "gpt-4.1-mini", "gpt-4.5-preview", "o1", "o3-mini", "o4-mini"} {
		if got := encodingFor(domain.ProviderCodex, model); got != tokenizer.O200kBase {
			t.Fatalf("%s: %s", model, got)
		}
	}
	for _, model := range []string{"gpt-4", "gpt-4-0613", "gpt-3.5-turbo", "gpt-35-turbo-0613"} {
		if got := encodingFor(domain.ProviderCodex, model); got != tokenizer.Cl100kBase {
			t.Fatalf("%s: %s", model, got)
		}
	}
}

func TestCounterCacheDoesNotAddPartialCountsOrMixEncodings(t *testing.T) {
	c := New()
	count := func(model, text string) domain.AgentTokenUsage {
		t.Helper()
		u, err := c.CountAgentTokens(context.Background(), domain.ProviderCodex, model, text)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	space, word, joined := count("gpt-5", " "), count("gpt-5", "hello"), count("gpt-5", " hello")
	if joined.Tokens != 1 || joined.Tokens >= space.Tokens+word.Tokens {
		t.Fatal("combined text must be recounted across BPE seams")
	}
	if cached := count("gpt-5", " hello"); cached != joined {
		t.Fatal("cache changed receipt")
	}
	x, y := count("gpt-5", "\uc548\ub155\ud558\uc138\uc694"), count("gpt-4", "\uc548\ub155\ud558\uc138\uc694")
	if x.Tokens == y.Tokens || x.Tokenizer == y.Tokenizer {
		t.Fatal("encoding cache collision")
	}
	for i := 0; i < cacheEntries+5; i++ {
		count("gpt-5", fmt.Sprintf("synthetic-%d", i))
	}
	if len(c.cache) != cacheEntries || c.lru.Len() != cacheEntries {
		t.Fatal("unbounded cache")
	}
	if afterEviction := count("gpt-5", " hello"); afterEviction != joined {
		t.Fatal("eviction changed count")
	}
}

func TestCounterConcurrentAndCancelledCalls(t *testing.T) {
	c := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CountAgentTokens(ctx, domain.ProviderCodex, "gpt-5", "hello"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// A waiting caller does not become an orphan background tokenization task.
	c.gate <- struct{}{}
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.CountAgentTokens(ctx, domain.ProviderCodex, "gpt-5", "hello"); done <- err }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-c.gate
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := c.CountAgentTokens(context.Background(), domain.ProviderCodex, "gpt-5", "Hello, world!")
			if err != nil || u.Tokens != 4 || !u.Exact {
				t.Errorf("concurrent count: %+v %v", u, err)
			}
		}()
	}
	wg.Wait()
}

func TestCounterBoundsPathologicalInputWithoutTruncation(t *testing.T) {
	c := New()
	for _, text := range []string{strings.Repeat("a", maxRunBytes+1), strings.Repeat(" ", maxRunBytes+1), strings.Repeat("\n", maxRunBytes+1), strings.Repeat("a ", maxTextBytes/2+1), strings.Repeat(strings.Repeat("a", maxRunBytes)+" ", 9), strings.Repeat("/\n", 8192), strings.Repeat("\u0301!", 8192)} {
		u, err := c.CountAgentTokens(context.Background(), domain.ProviderCodex, "gpt-5", text)
		if err != nil || u.Exact || u.Tokens != len(text) || u.Tokenizer != domain.UTF8ByteBoundCounter || u.Reason != "tokenizer_work_limit" {
			t.Fatalf("unbounded/sliced input: %+v %v", u, err)
		}
	}
	if len(c.cache) != 0 {
		t.Fatal("retained rejected input")
	}
}

func TestCounterDoesNotLabelUnicodeVersionDriftExact(t *testing.T) {
	// Official Python tiktoken 0.12.0 counts U+A7CB followed by '.b' as four
	// ordinary tokens; the Go library counts five. Rendered JSON retains the
	// mismatch (eight versus nine), so guard the entire input, not only raw text.
	for _, model := range []string{"gpt-5", "gpt-4"} {
		for _, text := range []string{"\ua7cb.b", "{\"text\":\"\ua7cb.b\"}", "\u1c89", "\U0001171e", "\ufb05", "\u1fe3", "\u1fd3", "\u0378"} {
			u, err := New().CountAgentTokens(context.Background(), domain.ProviderCodex, model, text)
			if err != nil || u.Exact || u.Reason != "unicode_classification_unavailable" || u.Tokens != len(text) || u.Tokenizer != domain.UTF8ByteBoundCounter {
				t.Fatalf("Unicode drift labelled exact: %+v %v", u, err)
			}
		}
	}
}

func TestCounterMeasuresEightHundredThousandTokensOffline(t *testing.T) {
	// tiktoken 0.12.0 encode_ordinary gives exactly one token per ' hello'.
	text := strings.Repeat(" hello", 800000)
	u, err := New().CountAgentTokens(context.Background(), domain.ProviderCodex, "gpt-5.4", text)
	if err != nil || u.Tokens != 800000 || !u.Exact {
		t.Fatalf("full-sized count: %+v %v", u, err)
	}
	if len(text) != 4800000 {
		t.Fatal("test no longer distinguishes bytes from tokens")
	}
}

func BenchmarkCounterFullText(b *testing.B) {
	text := strings.Repeat(" hello", 800000)
	c := New()
	// Warm the vocabulary, then vary the prompt so the benchmark is not a
	// receipt-cache hit. Count does not allocate a token-ID array.
	_, _ = c.CountAgentTokens(context.Background(), domain.ProviderCodex, "gpt-5", "warm")
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.CountAgentTokens(context.Background(), domain.ProviderCodex, "gpt-5", text+fmt.Sprint(i)); err != nil {
			b.Fatal(err)
		}
	}
}
