package domain

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestStoredConversationVerifierWarmOccurrences(t *testing.T) {
	// One event spans repeated chunks; the loader reuses its scratch buffer.
	stream := []byte(`{"blocks":[{"text":"` + strings.Repeat("a", 3*ConversationManifestChunkBytes) + `","type":"text"}],"kind":"message","role":"user","seq":0}`)
	m, bodies := conversationTestPlan(t, conversationTestEnvelope(t, false), stream, 1)
	h, _ := ConversationManifestHash(m)
	if m.Chunks[1].Hash != m.Chunks[2].Hash {
		t.Fatal("fixture must repeat a chunk")
	}
	var v StoredDocumentVerifier
	scratch := make([]byte, ConversationManifestChunkBytes)
	missing := errors.New("current occurrence missing")
	for _, mode := range []string{"cold", "warm", "corrupt-repeat", "missing-repeat"} {
		t.Run(mode, func(t *testing.T) {
			before := storedVerifierProofSnapshot(&v)
			calls := 0
			err := v.VerifyConversation(context.Background(), h, m, func(_ context.Context, key ContentHash) ([]byte, error) {
				calls++
				if key != m.Chunks[calls-1].Hash {
					t.Fatal("chunk occurrence order changed")
				}
				n := copy(scratch, bodies[key])
				if calls == 3 {
					if mode == "corrupt-repeat" {
						scratch[0] ^= 1
					}
					if mode == "missing-repeat" {
						return nil, missing
					}
				}
				return scratch[:n], nil
			})
			if mode == "cold" || mode == "warm" {
				if err != nil || calls != len(m.Chunks) {
					t.Fatalf("every occurrence must be loaded: calls=%d err=%v", calls, err)
				}
				if seq, ok := v.lookup(storedEventKey{sha256.Sum256(stream), "2"}); !ok || seq != 0 || v.firstBytes != 0 {
					t.Fatal("root semantic proof missing or legacy hint changed")
				}
			} else {
				if calls != 3 || (mode == "corrupt-repeat" && !errors.Is(err, ErrConversationManifest)) || (mode == "missing-repeat" && !errors.Is(err, missing)) {
					t.Fatalf("warm current-byte failure lost: calls=%d err=%v", calls, err)
				}
			}
			if mode != "cold" && !storedVerifierProofEqual(before, storedVerifierProofSnapshot(&v)) {
				t.Fatal("warm hit or failed root changed proofs")
			}
		})
	}
}

func TestStoredConversationVerifierWarmSemantics(t *testing.T) {
	ctx := context.Background()
	env := conversationTestEnvelope(t, true)
	m, bodies := conversationTestPlan(t, env, []byte(conversationVectorStream), 2)
	h, _ := ConversationManifestHash(m)
	load := func(_ context.Context, key ContentHash) ([]byte, error) { return bodies[key], nil }
	var v StoredDocumentVerifier
	cold := &conversationCancelContext{Context: ctx}
	warm := &conversationCancelContext{Context: ctx}
	if err := v.VerifyConversation(cold, h, m, load); err != nil {
		t.Fatal(err)
	}
	if err := v.VerifyConversation(warm, h, m, load); err != nil {
		t.Fatal(err)
	}
	// A hit bypasses only the typed semantic verifier's cancellation checkpoints.
	// This is an execution-path assertion, not a duration or speedup assertion.
	if warm.calls >= cold.calls || len(v.events) != 2 {
		t.Fatal("warm root did not reuse exact event proofs", cold.calls, warm.calls)
	}
	for _, tc := range []struct {
		name, stream string
		count        int64
	}{
		{"order", conversationVectorStream + "," + conversationVectorStream, 4},
		{"count-low", conversationVectorStream, 1},
		{"count-high", conversationVectorStream, 3},
		{"trailing-comma", conversationVectorStream + ",", 2},
		{"missing-comma", strings.Replace(conversationVectorStream, "},{", "}{", 1), 2},
		{"duplicate", strings.Replace(conversationVectorStream, `"seq":1`, `"seq":1,"seq":1`, 1), 2},
		{"precision", strings.Replace(conversationVectorStream, "9007199254740992", "9007199254740993", 1), 2},
		{"escape", strings.Replace(conversationVectorStream, `\u003c`, "<", 1), 2},
		{"UTF8", strings.Replace(conversationVectorStream, "\uD55C\uAE00", string([]byte{0xff}), 1), 2},
		{"late-new-invalid", conversationVectorStream + `,{"kind":"turn","role":"user","seq":2},{"kind":"unknown","seq":3}`, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, chunks := conversationTestPlan(t, env, []byte(tc.stream), tc.count)
			want, _ := ConversationManifestHash(candidate)
			read := func(_ context.Context, key ContentHash) ([]byte, error) { return chunks[key], nil }
			before := storedVerifierProofSnapshot(&v)
			for _, verifier := range []*StoredDocumentVerifier{nil, &v} {
				if err := verifier.VerifyConversation(ctx, want, candidate, read); !errors.Is(err, ErrConversationManifest) {
					t.Fatal("stateless/warm acceptance differs", err)
				}
			}
			if !storedVerifierProofEqual(before, storedVerifierProofSnapshot(&v)) {
				t.Fatal("failed root admitted or evicted proofs")
			}
		})
	}
}

func TestStoredConversationVerifierBoundedPending(t *testing.T) {
	var stream strings.Builder
	for i := 0; i < storedEventProofLimit+1; i++ {
		if i > 0 {
			stream.WriteByte(',')
		}
		fmt.Fprintf(&stream, `{"kind":"turn","role":"user","seq":%d}`, i)
	}
	var v StoredDocumentVerifier
	scanner := conversationEventScanner{version: "1", limit: storedEventProofLimit + 1, verifier: &v}
	if err := scanner.add(context.Background(), []byte(stream.String())); err != nil {
		t.Fatal(err)
	}
	if len(scanner.pending) != storedEventProofLimit || len(v.events) != 0 || len(v.order) != 0 || v.firstBytes != 0 {
		t.Fatal("root pending proofs unbounded or admitted before whole-root success")
	}
	// The same bounded admission implementation is shared with legacy; exercise
	// a root that exceeds it, then a new tail to force exactly one eviction.
	m, bodies := conversationTestPlan(t, []byte(storedVerifierEnvelope(t, "1")), []byte(stream.String()), storedEventProofLimit+1)
	h, _ := ConversationManifestHash(m)
	if err := v.VerifyConversation(context.Background(), h, m, func(_ context.Context, key ContentHash) ([]byte, error) { return bodies[key], nil }); err != nil {
		t.Fatal(err)
	}
	if len(v.events) != storedEventProofLimit || len(v.order) != storedEventProofLimit || v.next != 0 || v.firstBytes != 0 {
		t.Fatal("root admission exceeded shared cache bound")
	}
	tail := []byte(fmt.Sprintf(`{"kind":"turn","role":"user","seq":%d}`, storedEventProofLimit))
	if _, found := v.lookup(storedEventKey{sha256.Sum256(tail), "1"}); found {
		t.Fatal("pending overflow was admitted")
	}
	m, bodies = conversationTestPlan(t, m.Envelope, tail, 1)
	h, _ = ConversationManifestHash(m)
	if err := v.VerifyConversation(context.Background(), h, m, func(_ context.Context, key ContentHash) ([]byte, error) { return bodies[key], nil }); err != nil {
		t.Fatal(err)
	}
	if len(v.events) != storedEventProofLimit || len(v.order) != storedEventProofLimit || v.next != 1 || v.firstBytes != 0 {
		t.Fatal("root eviction changed bound or legacy hint")
	}
}
