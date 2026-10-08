package domain

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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

func TestStoredConversationVerifierLegacyAndVersion(t *testing.T) {
	ctx := context.Background()
	env := storedVerifierEnvelope(t, "1")
	for _, event := range []string{
		`{"seq":0,"role":"user","unknown":true,"kind":"turn"}`,
		`{"kind":"turn","role":"user","seq":0,"seq":0}`,
		`{"call_id":"x","input":{"n":1e0},"kind":"tool_call","seq":0,"tool_name":"test"}`,
		`{"call_id":"x","input":{"n":9007199254740993},"kind":"tool_call","seq":0,"tool_name":"test"}`,
	} {
		var doc CIRDocument
		raw := storedVerifierDoc(env, event)
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		normalized, err := CanonicalBytes(doc)
		if err != nil {
			t.Fatal(err)
		}
		var parts struct{ Events []json.RawMessage }
		if err := json.Unmarshal(normalized, &parts); err != nil {
			t.Fatal(err)
		}
		var v StoredDocumentVerifier
		if err := v.Verify(ctx, HashContent(normalized), raw); err != nil {
			t.Fatal(err)
		}
		before := storedVerifierProofSnapshot(&v)
		for i, stream := range [][]byte{parts.Events[0], []byte(event)} {
			m, bodies := conversationTestPlan(t, []byte(env), stream, 1)
			h, _ := ConversationManifestHash(m)
			err := v.VerifyConversation(ctx, h, m, func(_ context.Context, key ContentHash) ([]byte, error) { return bodies[key], nil })
			if (i == 0 && err != nil) || (i == 1 && !errors.Is(err, ErrConversationManifest)) {
				t.Fatal("legacy normalization leaked into root acceptance", i, err)
			}
			if !storedVerifierProofEqual(before, storedVerifierProofSnapshot(&v)) {
				t.Fatal("shared proof changed cache/hint or admitted noncanonical bytes")
			}
		}
	}
	var v StoredDocumentVerifier
	event := `{"agent_message":false,"blocks":[],"kind":"message","role":"user","seq":0}`
	for _, version := range []string{"2", "1"} {
		m, bodies := conversationTestPlan(t, []byte(storedVerifierEnvelope(t, version)), []byte(event), 1)
		h, _ := ConversationManifestHash(m)
		before := storedVerifierProofSnapshot(&v)
		err := v.VerifyConversation(ctx, h, m, func(_ context.Context, key ContentHash) ([]byte, error) { return bodies[key], nil })
		if version == "2" && err != nil {
			t.Fatal(err)
		}
		if version == "1" && (!errors.Is(err, ErrConversationManifest) || !storedVerifierProofEqual(before, storedVerifierProofSnapshot(&v))) {
			t.Fatal("root v2 proof authorized v1 fields or altered cache", err)
		}
	}
	// Legacy's implicit version is not a root v1 proof even for identical bytes.
	event = `{"kind":"turn","role":"user","seq":0}`
	raw := storedVerifierDoc(storedVerifierEnvelope(t, ""), event)
	if err := v.Verify(ctx, HashContent(raw), raw); err != nil {
		t.Fatal(err)
	}
	key := storedEventKey{sha256.Sum256([]byte(event)), "1"}
	if _, found := v.lookup(key); found {
		t.Fatal("implicit legacy version aliased to v1")
	}
	m, bodies := conversationTestPlan(t, []byte(env), []byte(event), 1)
	h, _ := ConversationManifestHash(m)
	if err := v.VerifyConversation(ctx, h, m, func(_ context.Context, key ContentHash) ([]byte, error) { return bodies[key], nil }); err != nil {
		t.Fatal(err)
	}
	if _, found := v.lookup(key); !found {
		t.Fatal("explicit v1 proof missing")
	}
}

func TestStoredConversationVerifierCanceledAdmission(t *testing.T) {
	ctx := context.Background()
	seed := storedVerifierCanonical(t, "2", Event{Kind: EventTurn, Seq: 0, Role: "user"})
	m, bodies := conversationTestPlan(t, conversationTestEnvelope(t, false), []byte(`{"kind":"turn","role":"user","seq":0},{"kind":"turn","role":"assistant","seq":1}`), 2)
	h, _ := ConversationManifestHash(m)
	load := func(_ context.Context, key ContentHash) ([]byte, error) { return bodies[key], nil }
	for _, seeded := range []bool{false, true} {
		var probe StoredDocumentVerifier
		if seeded {
			if err := probe.Verify(ctx, HashContent(seed), seed); err != nil {
				t.Fatal(err)
			}
		}
		observed := &conversationCancelContext{Context: ctx}
		if err := probe.VerifyConversation(observed, h, m, load); err != nil {
			t.Fatal(err)
		}
		for _, at := range []int{1, observed.calls / 2, observed.calls} {
			t.Run(fmt.Sprintf("seeded-%t-check-%d", seeded, at), func(t *testing.T) {
				var v StoredDocumentVerifier
				if seeded {
					if err := v.Verify(ctx, HashContent(seed), seed); err != nil {
						t.Fatal(err)
					}
				}
				before := storedVerifierProofSnapshot(&v)
				canceled, cancel := context.WithCancel(ctx)
				defer cancel()
				hooked := &conversationCancelContext{Context: canceled, cancel: cancel, at: at}
				if err := v.VerifyConversation(hooked, h, m, load); !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation lost", err)
				}
				if !storedVerifierProofEqual(before, storedVerifierProofSnapshot(&v)) {
					t.Fatal("canceled root changed cache/hint")
				}
			})
		}
	}
	// Cancellation while admission waits for the metadata mutex cannot publish.
	var v StoredDocumentVerifier
	before := storedVerifierProofSnapshot(&v)
	canceled, cancel := context.WithCancel(ctx)
	v.mu.Lock()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		done <- v.remember(canceled, []storedEventProof{{key: storedEventKey{sha256.Sum256([]byte("pending")), "2"}, seq: 1}}, nil)
	}()
	<-started
	cancel()
	v.mu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !storedVerifierProofEqual(before, storedVerifierProofSnapshot(&v)) {
		t.Fatal("canceled admission changed cache")
	}
}

func TestStoredConversationVerifierConcurrentHint(t *testing.T) {
	ctx := context.Background()
	var v StoredDocumentVerifier
	seed := storedVerifierCanonical(t, "1", Event{Kind: EventTurn, Seq: 0, Role: "user"})
	if err := v.Verify(ctx, HashContent(seed), seed); err != nil {
		t.Fatal(err)
	}
	m, bodies := conversationTestPlan(t, conversationTestEnvelope(t, false), []byte(conversationVectorStream), 2)
	h, _ := ConversationManifestHash(m)
	load := func(_ context.Context, key ContentHash) ([]byte, error) { return bodies[key], nil }
	for _, legacy := range [][]byte{storedVerifierCanonical(t, "1", Event{Kind: EventTurn, Seq: 10, Role: "assistant"}), storedVerifierCanonical(t, "1")} {
		entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			done <- v.VerifyConversation(ctx, h, m, func(ctx context.Context, key ContentHash) ([]byte, error) {
				close(entered)
				<-resume
				return load(ctx, key)
			})
		}()
		<-entered
		err := v.Verify(ctx, HashContent(legacy), legacy)
		hint := storedVerifierProofSnapshot(&v).firstBytes
		close(resume)
		if rootErr := <-done; rootErr != nil {
			t.Fatal(rootErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		if v.firstBytes != hint {
			t.Fatal("root admission overwrote concurrent legacy hint")
		}
	}
	empty, _ := conversationTestPlan(t, conversationTestEnvelope(t, false), nil, 0)
	emptyHash, _ := ConversationManifestHash(empty)
	if err := v.VerifyConversation(ctx, emptyHash, empty, nil); err != nil || v.firstBytes != 0 {
		t.Fatal("empty root changed explicit zero hint", err)
	}
	// Mixed readers share one verifier; root-seeded canonical events also remain
	// valid through legacy verification, whose predictor is independent.
	legacy := storedVerifierDoc(string(m.Envelope), conversationVectorStream)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				if err := v.VerifyConversation(ctx, h, m, load); err != nil {
					t.Error(err)
				}
				if err := v.Verify(ctx, HashContent(legacy), legacy); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
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
