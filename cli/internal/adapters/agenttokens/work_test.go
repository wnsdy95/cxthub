package agenttokens

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/tiktoken-go/tokenizer"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestPieceWorkUsesEncodingBoundariesAndAggregateBytes(t *testing.T) {
	for _, encoding := range []tokenizer.Encoding{tokenizer.O200kBase, tokenizer.Cl100kBase} {
		t.Run(string(encoding), func(t *testing.T) {
			c := New()
			for _, tc := range []struct{ name, text, reason string }{
				{"per-piece exact bound", strings.Repeat("a", maxRunBytes), ""},
				{"per-piece overflow", strings.Repeat("a", maxRunBytes+1), "tokenizer_work_limit"},
				{"UTF8 bytes", strings.Repeat("\u754c", maxRunBytes/3+1), "tokenizer_work_limit"},
				// A preceding space belongs to the next word piece. Every piece
				// is <=4096 bytes; only the ninth crosses aggregate work.
				{"aggregate within", strings.Repeat(strings.Repeat("a", maxRunBytes-1)+" ", 8), ""},
				{"aggregate over", strings.Repeat(strings.Repeat("a", maxRunBytes-1)+" ", 9), "tokenizer_work_limit"},
				{"numeric boundaries", strings.Repeat("1234567890", 1000), ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					reason, err := c.inputFallbackReason(context.Background(), encoding, tc.text)
					if err != nil || reason != tc.reason {
						t.Fatalf("reason=%q want=%q err=%v", reason, tc.reason, err)
					}
				})
			}
			reason, err := c.inputFallbackReason(context.Background(), encoding, strings.Repeat("\u0301!", 8192))
			want := ""
			if encoding == tokenizer.Cl100kBase {
				want = "tokenizer_work_limit"
			}
			if err != nil || reason != want {
				t.Fatalf("encoding-specific mark pieces: %q %v", reason, err)
			}
		})
	}
}

func TestPiecePreflightRejectsGapsEmptyMatchesAndTimeout(t *testing.T) {
	for _, tc := range []struct{ pattern, text string }{
		{"a+", "aaXaa"}, {"a*", "b"}, {"a+", "aaaX"},
		// Deliberately pathological test-only regex; production uses pinned
		// tokenizer patterns. The private error text must never escape.
		{"(a+)+$", strings.Repeat("a", 10000) + "!private"},
	} {
		re := regexp2.MustCompile(tc.pattern, regexp2.None)
		re.MatchTimeout = time.Millisecond
		reason, err := checkPieceWork(context.Background(), re, tc.text)
		if err != nil || reason != "tokenizer_work_limit" {
			t.Fatalf("reason=%q err=%v", reason, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if reason, err = checkPieceWork(ctx, re, tc.text); !errors.Is(err, context.Canceled) || reason != "" {
			t.Fatalf("cancelled preflight: reason=%q err=%v", reason, err)
		}
	}
}

type observedCodec struct {
	tokenizer.Codec
	entered, release chan struct{}
}

func (c observedCodec) Count(text string) (int, error) {
	close(c.entered)
	<-c.release
	return c.Codec.Count(text)
}

func TestCounterCancellationDuringBoundedLibraryCount(t *testing.T) {
	c := New()
	underlying, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		t.Fatal(err)
	}
	observed := observedCodec{Codec: underlying, entered: make(chan struct{}), release: make(chan struct{})}
	c.codecs[tokenizer.O200kBase] = observed
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.CountAgentTokens(ctx, domain.ProviderCodex, "gpt-5", strings.Repeat(" hello", 800000))
		done <- err
	}()
	select {
	case <-observed.entered:
	case err := <-done:
		t.Fatalf("preflight did not reach library count: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("preflight did not reach library count")
	}
	start := time.Now()
	cancel()
	close(observed.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("bounded library count did not finish after cancellation")
	}
	t.Logf("remaining synchronous count after cancellation: %s", time.Since(start))
	if len(c.cache) != 0 || len(c.gate) != 0 {
		t.Fatal("cancelled count retained a receipt or active computation")
	}
}
