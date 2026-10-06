package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
)

// This oracle deliberately does not use the candidate's parser or cache.
func storedVerifierStoredOracle(want ContentHash, raw []byte) error {
	var doc CIRDocument
	if json.Unmarshal(raw, &doc) != nil {
		return ErrInvalidCIR
	}
	return ValidateSessionDocHash(SessionDoc{Hash: want, CIR: doc})
}
func storedVerifierErrorClass(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, ErrInvalidCIR):
		return "invalid-CIR"
	case errors.Is(err, ErrHashMismatch):
		return "hash-mismatch"
	case errors.Is(err, ErrUnsupportedCIRVersion):
		return "unsupported-version"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return err.Error()
	}
}
func storedVerifierCanonical(t testing.TB, version string, events ...Event) []byte {
	t.Helper()
	raw, err := CanonicalBytes(CIRDocument{Envelope: Envelope{CIRVersion: version}, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func storedVerifierEnvelope(t testing.TB, version string) string {
	t.Helper()
	var doc struct{ Envelope json.RawMessage }
	if err := json.Unmarshal(storedVerifierCanonical(t, version), &doc); err != nil {
		t.Fatal(err)
	}
	return string(doc.Envelope)
}
func storedVerifierDoc(env, events string) []byte {
	return []byte(`{"envelope":` + env + `,"events":[` + events + `]}`)
}
func storedVerifierAssertParity(t testing.TB, v *StoredDocumentVerifier, want ContentHash, raw []byte) {
	t.Helper()
	expected := storedVerifierStoredOracle(want, raw)
	actual := v.Verify(context.Background(), want, raw)
	if storedVerifierErrorClass(actual) != storedVerifierErrorClass(expected) {
		t.Fatalf("verification differs: got %v; oracle %v", actual, expected)
	}
}

func TestStoredDocumentVerifierDifferential(t *testing.T) {
	env1, env2 := storedVerifierEnvelope(t, "1"), storedVerifierEnvelope(t, "2")
	a := `{"kind":"turn","role":"user","seq":0}`
	b := `{"kind":"turn","role":"assistant","seq":1}`
	message := `{"agent_message":false,"blocks":[],"kind":"message","role":"user","seq":0}`
	compact := `{"kind":"compaction","replacement":[],"replacement_complete":false,"seq":0}`
	cases := []struct {
		name string
		raw  []byte
	}{
		{"canonical", storedVerifierDoc(env1, a+","+b)},
		{"equal-negative-sequences", storedVerifierDoc(env1, strings.ReplaceAll(strings.ReplaceAll(a+","+b, `"seq":0`, `"seq":-1`), `"seq":1`, `"seq":-1`))},
		{"nested-sorting", storedVerifierDoc(env2, `{"kind":"compaction","replacement":[`+b+`,`+a+`],"replacement_complete":true,"seq":2}`)},
		{"unsorted-top-events", storedVerifierDoc(env1, b+","+a)},
		{"nested-new-tail-sorting", storedVerifierDoc(env2, a+`,{"kind":"compaction","replacement":[`+b+`,`+a+`],"replacement_complete":true,"seq":2}`)},
		{"unknown-event-field", storedVerifierDoc(env1, strings.TrimSuffix(a, "}")+`,"unknown":true}`)},
		{"unknown-envelope-field", storedVerifierDoc(strings.TrimSuffix(env1, "}")+`,"unknown":true}`, a)},
		{"unknown-root", append(bytes.Clone(storedVerifierDoc(env1, a))[:len(storedVerifierDoc(env1, a))-1], []byte(`,"unknown":true}`)...)},
		{"event-duplicate", storedVerifierDoc(env1, strings.TrimSuffix(a, "}")+`,"seq":0}`)},
		{"event-earlier-invalid-type", storedVerifierDoc(env1, `{"seq":"bad","kind":"turn","role":"user","seq":0}`)},
		{"trailing-envelope-override", storedVerifierDoc(env1, a+`],"envelope":{"cir_version":"999"},"padding":[`)},
		{"trailing-envelope-case-fold", storedVerifierDoc(env1, a+`],"ENVELOPE":{"cir_version":"999"},"padding":[`)},
		{"trailing-events-override", storedVerifierDoc(env1, a+`],"events":[`)},
		{"trailing-events-unicode-fold", storedVerifierDoc(env1, a+`],"event\u017f":[`)},
		{"null-duplicate-envelope", storedVerifierDoc(env1, a+`],"envelope":null,"padding":[`)},
		{"duplicate-root-earlier-type-error", []byte(`{"envelope":{"cir_version":42},"envelope":` + env1 + `,"events":[` + a + `]}`)},
		{"root-reordered", []byte(`{"events":[` + a + `],"envelope":` + env1 + `}`)},
		{"empty-null-arrays", []byte(`{"envelope":null,"events":null}`)},
		{"explicit-v2-false", storedVerifierDoc(env2, message)},
		{"v1-explicit-v2-false", storedVerifierDoc(env1, message)},
		{"empty-replacement", storedVerifierDoc(env2, compact)},
		{"v1-empty-replacement", storedVerifierDoc(env1, compact)},
		{"provider-metadata-null", storedVerifierDoc(env2, strings.TrimSuffix(a, "}")+`,"provider_metadata":null}`)},
		{"locked-null", storedVerifierDoc(env2, `{"kind":"reasoning","locked":null,"seq":0}`)},
		{"wrong-union-field", storedVerifierDoc(env2, strings.TrimSuffix(a, "}")+`,"replacement":[]}`)},
		{"numeric-rounding", storedVerifierDoc(env1, `{"call_id":"a","kind":"tool_result","output":9007199254740993,"seq":0}`)},
		{"number-exponent", storedVerifierDoc(env1, `{"call_id":"a","kind":"tool_result","output":1e0,"seq":0}`)},
		{"provider-number-precision", storedVerifierDoc(env2, `{"kind":"turn","provider_metadata":{"create_time":9007199254740993},"role":"user","seq":0}`)},
		{"escaped-equivalent-string", storedVerifierDoc(env1, strings.Replace(a, `"user"`, `"\u0075ser"`, 1))},
		{"quoted-framing", storedVerifierDoc(env1, `{"kind":"turn","role":"braces } ] [ { comma , quote \" slash \\","seq":0}`)},
		{"envelope-leading-space", storedVerifierDoc(" "+env1, a)},
		{"inter-event-space", storedVerifierDoc(env1, a+", "+b)},
		{"trailing-newline", append(storedVerifierDoc(env1, a), '\n')},
		{"unsupported-version", storedVerifierDoc(storedVerifierEnvelope(t, "1"), a)},
		{"later-type-error-before-version", storedVerifierDoc(strings.Replace(env1, `"1"`, `"999"`, 1), `{"kind":"turn","seq":"bad"}`)},
		{"late-feature-error-before-union-error", storedVerifierDoc(env1, `{"kind":"bogus","seq":0},`+compact)},
		{"trailing-comma", storedVerifierDoc(env1, a+",")},
		{"leading-comma", storedVerifierDoc(env1, ","+a)},
		{"double-comma", storedVerifierDoc(env1, a+",,"+b)},
		{"missing-comma", storedVerifierDoc(env1, a+b)},
		{"mismatched-brackets", storedVerifierDoc(env1, `{"kind":"turn","seq":0]`)},
		{"unterminated-string", storedVerifierDoc(env1, `{"kind":"turn","role":"x}`)},
		{"extra-root", append(storedVerifierDoc(env1, a), []byte(` {}`)...)},
		{"event-null", storedVerifierDoc(env1, `null`)},
	}
	for i := range cases {
		if cases[i].name == "unsupported-version" {
			cases[i].raw = bytes.Replace(cases[i].raw, []byte(`"cir_version":"1"`), []byte(`"cir_version":"999"`), 1)
		}
	}
	seed := storedVerifierDoc(env2, message+`,`+`{"kind":"compaction","replacement":[],"replacement_complete":false,"seq":1}`)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wants := []ContentHash{HashContent(tc.raw)}
			var doc CIRDocument
			var normalized []byte
			if json.Unmarshal(tc.raw, &doc) == nil {
				if canonical, err := CanonicalBytes(doc); err == nil {
					normalized = canonical
					if h := HashContent(canonical); h != wants[0] {
						wants = append(wants, h)
					}
				}
			}
			for _, want := range wants {
				var cold, warm StoredDocumentVerifier
				if err := warm.Verify(context.Background(), HashContent(seed), seed); err != nil {
					t.Fatal(err)
				}
				// Ensure malformed suffixes exercise a proven prefix, not only the
				// first-event-miss fallback.
				for _, prefix := range [][]byte{storedVerifierDoc(env1, a+","+b), storedVerifierDoc(env2, a+","+b)} {
					if err := warm.Verify(context.Background(), HashContent(prefix), prefix); err != nil {
						t.Fatal(err)
					}
				}
				if normalized != nil {
					if err := warm.Verify(context.Background(), HashContent(normalized), normalized); err != nil {
						t.Fatal(err)
					}
				}
				storedVerifierAssertParity(t, &cold, want, tc.raw)
				storedVerifierAssertParity(t, &warm, want, tc.raw)
				storedVerifierAssertParity(t, &warm, want, tc.raw)
			}
		})
	}
}

func TestStoredDocumentVerifierWarmMutationAndVersion(t *testing.T) {
	raw := storedVerifierCanonical(t, "2", Event{Kind: EventMessage, Seq: 0, Role: "user", AgentMessage: true, AgentAuthor: "agent"})
	want := HashContent(raw)
	var v StoredDocumentVerifier
	if err := v.Verify(context.Background(), want, raw); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(bytes.Clone(raw), []byte(`"role":"user"`), []byte(`"role":"assistant"`), 1)
	storedVerifierAssertParity(t, &v, want, changed)
	storedVerifierAssertParity(t, &v, HashContent(changed), changed)
	for _, version := range []string{"1", "", "999"} {
		candidate := bytes.Replace(raw, []byte(`"cir_version":"2"`), []byte(`"cir_version":"`+version+`"`), 1)
		storedVerifierAssertParity(t, &v, HashContent(candidate), candidate)
	}
	// A cached proof must not reference caller-owned mutable bytes.
	raw[0] = '!'
	storedVerifierAssertParity(t, &v, want, raw)
}

func TestStoredDocumentVerifierGlobalDepth(t *testing.T) {
	env := storedVerifierEnvelope(t, "1")
	for _, depth := range []int{9997, 9998} {
		t.Run(fmt.Sprint(depth+3), func(t *testing.T) {
			nested := strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth)
			raw := storedVerifierDoc(env, `{"call_id":"a","kind":"tool_result","output":`+nested+`,"seq":0}`)
			var v StoredDocumentVerifier
			seed := storedVerifierCanonical(t, "1", Event{Kind: EventTurn, Seq: 0, Role: "user"})
			if err := v.Verify(context.Background(), HashContent(seed), seed); err != nil {
				t.Fatal(err)
			}
			before := storedVerifierProofSnapshot(&v)
			storedVerifierAssertParity(t, &v, HashContent(raw), raw)
			storedVerifierAssertParity(t, &v, HashContent(raw), raw)
			if depth == 9998 && !storedVerifierProofEqual(before, storedVerifierProofSnapshot(&v)) {
				t.Fatal("excessive depth admitted an event proof")
			}
		})
	}
}

type storedVerifierProofState struct {
	events     map[storedEventKey]int
	order      []storedEventKey
	next       int
	firstBytes int
}

func storedVerifierProofSnapshot(v *StoredDocumentVerifier) storedVerifierProofState {
	v.mu.Lock()
	defer v.mu.Unlock()
	return storedVerifierProofState{maps.Clone(v.events), slices.Clone(v.order), v.next, v.firstBytes}
}
func storedVerifierProofEqual(a, b storedVerifierProofState) bool {
	return maps.Equal(a.events, b.events) && slices.Equal(a.order, b.order) && a.next == b.next && a.firstBytes == b.firstBytes
}

type storedVerifierCheckContext struct {
	context.Context
	check func()
}

func (c storedVerifierCheckContext) Err() error { c.check(); return c.Context.Err() }

func TestStoredDocumentVerifierFailedAdmissionAndCancel(t *testing.T) {
	env := storedVerifierEnvelope(t, "1")
	seed := storedVerifierCanonical(t, "1", Event{Kind: EventTurn, Seq: 0, Role: "user"})
	valid := storedVerifierCanonical(t, "1", Event{Kind: EventTurn, Seq: 0, Role: "user"}, Event{Kind: EventTurn, Seq: 1, Role: "user"})
	for _, tc := range []struct {
		name string
		raw  []byte
		want ContentHash
	}{
		{"late-unknown-field", bytes.Replace(valid, []byte(`"seq":1}`), []byte(`"seq":1,"unknown":true}`), 1), ""},
		{"late-regression", bytes.Replace(valid, []byte(`"seq":1}`), []byte(`"seq":-1}`), 1), ""},
		{"late-malformed", storedVerifierDoc(env, `{"kind":"turn","role":"user","seq":0},{]`), ""},
		{"wrong-identity", valid, HashContent([]byte("different"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v StoredDocumentVerifier
			if err := v.Verify(context.Background(), HashContent(seed), seed); err != nil {
				t.Fatal(err)
			}
			before := storedVerifierProofSnapshot(&v)
			if tc.want == "" {
				tc.want = HashContent(tc.raw)
			}
			storedVerifierAssertParity(t, &v, tc.want, tc.raw)
			if storedVerifierStoredOracle(tc.want, tc.raw) == nil {
				t.Fatal("bad failure fixture")
			}
			if !storedVerifierProofEqual(before, storedVerifierProofSnapshot(&v)) {
				t.Fatal("failed document admitted or evicted proofs")
			}
		})
	}
	// Find the last checkpoint before admission without assuming parser internals.
	var probe StoredDocumentVerifier
	checks, lastBeforeAdmission := 0, 0
	observed := storedVerifierCheckContext{Context: context.Background(), check: func() {
		checks++
		if len(storedVerifierProofSnapshot(&probe).events) == 0 {
			lastBeforeAdmission = checks
		}
	}}
	if err := probe.Verify(observed, HashContent(valid), valid); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int{1, lastBeforeAdmission / 2, lastBeforeAdmission} {
		t.Run(fmt.Sprintf("cancel-%d", at), func(t *testing.T) {
			var v StoredDocumentVerifier
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			hooked := storedVerifierCheckContext{Context: ctx, check: func() {
				calls++
				if calls == at {
					cancel()
				}
			}}
			if err := v.Verify(hooked, HashContent(valid), valid); !errors.Is(err, context.Canceled) {
				t.Fatal("cancel ignored", err)
			}
			if len(v.events) != 0 || len(v.order) != 0 || v.next != 0 || v.firstBytes != 0 {
				t.Fatal("canceled scan admitted proofs")
			}
		})
	}
}

func TestStoredDocumentVerifierCapacity(t *testing.T) {
	// The input is about 3 MiB of tiny synthetic events, not a timing benchmark.
	var events strings.Builder
	for i := 0; i < storedEventProofLimit+3; i++ {
		if i > 0 {
			events.WriteByte(',')
		}
		fmt.Fprintf(&events, `{"kind":"turn","role":"user","seq":%d}`, i)
	}
	raw := storedVerifierDoc(storedVerifierEnvelope(t, "1"), events.String())
	var v StoredDocumentVerifier
	if err := v.Verify(context.Background(), HashContent(raw), raw); err != nil {
		t.Fatal(err)
	}
	if len(v.events) != storedEventProofLimit || len(v.order) != storedEventProofLimit {
		t.Fatal("cache capacity not enforced")
	}
	next := storedVerifierCanonical(t, "1", Event{Kind: EventTurn, Seq: storedEventProofLimit + 10, Role: "user"})
	if err := v.Verify(context.Background(), HashContent(next), next); err != nil {
		t.Fatal(err)
	}
	if len(v.events) != storedEventProofLimit || len(v.order) != storedEventProofLimit || v.next != 1 {
		t.Fatal("bounded admission/eviction broken")
	}
	// The evicted event must still validate, and repeated IDs consume one slot.
	dup := storedVerifierDoc(storedVerifierEnvelope(t, "1"), `{"kind":"turn","role":"user","seq":0},{"kind":"turn","role":"user","seq":0}`)
	storedVerifierAssertParity(t, &v, HashContent(dup), dup)
	if len(v.events) != storedEventProofLimit || v.next != 2 {
		t.Fatal("duplicate admission consumed multiple slots")
	}
}

func TestStoredDocumentVerifierConcurrent(t *testing.T) {
	var v StoredDocumentVerifier
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		raw := storedVerifierCanonical(t, "1", Event{Kind: EventTurn, Seq: i, Role: "user"})
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 8; j++ {
				storedVerifierAssertParity(t, &v, HashContent(raw), raw)
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(v.events) != 8 || len(v.order) != 8 {
		t.Fatal("concurrent admission lost or duplicated proofs")
	}
}

func TestStoredDocumentVerifierVersionProofKeys(t *testing.T) {
	event := `{"agent_message":false,"blocks":[],"kind":"message","role":"user","seq":0}`
	raw := storedVerifierDoc(storedVerifierEnvelope(t, "2"), event)
	var v StoredDocumentVerifier
	if err := v.Verify(context.Background(), HashContent(raw), raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := v.events[storedEventKey{hash: sha256.Sum256([]byte(event)), version: "2"}]; !ok {
		t.Fatal("fixture did not prime v2 proof")
	}
	before := storedVerifierProofSnapshot(&v)
	v1 := storedVerifierDoc(storedVerifierEnvelope(t, "1"), event)
	if err := v.Verify(context.Background(), HashContent(v1), v1); !errors.Is(err, ErrInvalidCIR) {
		t.Fatal("v2 proof authorized v1 fields", err)
	}
	if !storedVerifierProofEqual(before, storedVerifierProofSnapshot(&v)) {
		t.Fatal("failed version reuse changed cache")
	}
}

func TestStoredDocumentVerifierLegacyFallbackCanonicalProofs(t *testing.T) {
	env := storedVerifierEnvelope(t, "1")
	event := `{"seq":0,"role":"user","unknown":true,"kind":"turn"}`
	raw := storedVerifierDoc(env, event)
	var doc CIRDocument
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
	want := HashContent(normalized)
	if want == HashContent(raw) {
		t.Fatal("fixture is canonical")
	}
	var v StoredDocumentVerifier
	if err := v.Verify(context.Background(), want, raw); err != nil {
		t.Fatal("legacy normalization rejected", err)
	}
	canonicalKey := storedEventKey{hash: sha256.Sum256(parts.Events[0]), version: "1"}
	rawKey := storedEventKey{hash: sha256.Sum256([]byte(event)), version: "1"}
	if _, ok := v.events[canonicalKey]; !ok {
		t.Fatal("successful fallback did not admit canonical proof")
	}
	if _, ok := v.events[rawKey]; ok {
		t.Fatal("legacy bytes were mislabeled canonical")
	}
	before := storedVerifierProofSnapshot(&v)
	storedVerifierAssertParity(t, &v, HashContent(raw), raw)
	if !storedVerifierProofEqual(before, storedVerifierProofSnapshot(&v)) {
		t.Fatal("raw identity mismatch admitted proofs")
	}
	storedVerifierAssertParity(t, &v, want, normalized)
}

func TestStoredDocumentVerifierWarmCancellation(t *testing.T) {
	first := Event{Kind: EventTurn, Seq: 0, Role: "user"}
	seed := storedVerifierCanonical(t, "1", first)
	raw := storedVerifierCanonical(t, "1", first, Event{Kind: EventTurn, Seq: 1, Role: "assistant"})
	var probe StoredDocumentVerifier
	if err := probe.Verify(context.Background(), HashContent(seed), seed); err != nil {
		t.Fatal(err)
	}
	checks, lastBeforeAdmission := 0, 0
	observed := storedVerifierCheckContext{Context: context.Background(), check: func() {
		checks++
		if len(storedVerifierProofSnapshot(&probe).events) == 1 {
			lastBeforeAdmission = checks
		}
	}}
	if err := probe.Verify(observed, HashContent(raw), raw); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int{1, lastBeforeAdmission / 2, lastBeforeAdmission} {
		t.Run(fmt.Sprintf("cancel-%d", at), func(t *testing.T) {
			var v StoredDocumentVerifier
			if err := v.Verify(context.Background(), HashContent(seed), seed); err != nil {
				t.Fatal(err)
			}
			before := storedVerifierProofSnapshot(&v)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			hooked := storedVerifierCheckContext{Context: ctx, check: func() {
				calls++
				if calls == at {
					cancel()
				}
			}}
			if err := v.Verify(hooked, HashContent(raw), raw); !errors.Is(err, context.Canceled) {
				t.Fatal("warm cancellation ignored", err)
			}
			if !storedVerifierProofEqual(before, storedVerifierProofSnapshot(&v)) {
				t.Fatal("canceled warm scan changed cache")
			}
		})
	}
}

func TestStoredDocumentVerifierProbeIsOnlyAHint(t *testing.T) {
	raw := storedVerifierCanonical(t, "1", Event{Kind: EventTurn, Seq: 0, Role: "user"}, Event{Kind: EventTurn, Seq: 1, Role: "assistant"})
	for _, hint := range []int{-1, 0, 1, len(raw), int(^uint(0) >> 1)} {
		t.Run(fmt.Sprint(hint), func(t *testing.T) {
			var v StoredDocumentVerifier
			if err := v.Verify(context.Background(), HashContent(raw), raw); err != nil {
				t.Fatal(err)
			}
			validLength := v.firstBytes
			v.firstBytes = hint
			changed := bytes.Replace(bytes.Clone(raw), []byte(`"role":"assistant"`), []byte(`"role":"changed"`), 1)
			before := storedVerifierProofSnapshot(&v)
			if err := v.Verify(context.Background(), HashContent(raw), changed); !errors.Is(err, ErrHashMismatch) {
				t.Fatal("hint authorized wrong identity", err)
			}
			if !storedVerifierProofEqual(before, storedVerifierProofSnapshot(&v)) {
				t.Fatal("failed probe changed cache or hint")
			}
			if err := v.Verify(context.Background(), HashContent(raw), raw); err != nil {
				t.Fatal("stale hint blocked valid bytes", err)
			}
			if v.firstBytes != validLength {
				t.Fatal("valid fallback did not refresh hint")
			}
		})
	}
}
