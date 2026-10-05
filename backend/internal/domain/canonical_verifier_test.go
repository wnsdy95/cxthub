package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func canonicalVerificationOracle(raw []byte) error {
	var cir CIRDocument
	if err := json.Unmarshal(raw, &cir); err != nil {
		return err
	}
	_, err := VerifySessionDoc(SessionDoc{Hash: HashContent(raw), CIR: cir})
	return err
}

func verifierFixture(t testing.TB) []byte {
	t.Helper()
	doc := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2"}, Events: []CIREvent{
		{Kind: EventMessage, Seq: 0, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "original <\uD55C\uAE00> \\\""}}},
		{Kind: EventToolCall, Seq: 1, CallID: "call", ToolName: "tool", Input: map[string]any{"n": json.Number("9007199254740992")}},
		{Kind: EventCompaction, Seq: 2, Replacement: []CIREvent{{Kind: EventMessage, Seq: 3, Role: RoleAssistant}, {Kind: EventMessage, Seq: 0, Role: RoleUser}}, ReplacementComplete: true},
	}}
	raw, err := CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCanonicalDocVerifierPreservesIdentityAndRejectsChangedSemantics(t *testing.T) {
	raw := verifierFixture(t)
	var v CanonicalDocVerifier
	ctx := context.Background()
	for _, candidate := range [][]byte{
		raw,
		bytes.Replace(raw, []byte(`9007199254740992`), []byte(`9007199254740993`), 1),
		bytes.Replace(raw, []byte(`"cir_version":"2"`), []byte(`"cir_version":"1"`), 1),
		bytes.Replace(raw, []byte(`"cir_version":"2"`), []byte(`"cir_version":"99"`), 1),
		bytes.Replace(raw, []byte(`"seq":1`), []byte(`"seq":-1`), 1),
		bytes.Replace(raw, []byte(`"seq":1`), []byte(`"seq":0`), 1), // stable equal sequences remain valid
		bytes.Replace(raw, []byte(`"kind":"tool_call"`), []byte(`"kind":"tool_call","unknown":true`), 1),
		bytes.Replace(raw, []byte(`"seq":1`), []byte(`"seq":1,"seq":1`), 1),
		bytes.Replace(raw, []byte(`"kind":"tool_call"`), []byte(`"kind":"message"`), 1),
		bytes.Replace(raw, []byte(`"cir_version":"2"`), []byte(`"cir_version": "2"`), 1),
		bytes.Replace(raw, []byte(`"blocks":[]`), []byte(`"blocks":null`), 1),
		append(append([]byte{}, raw...), '\n'),
		[]byte(`{"envelope":{},"events":[]}`),
		[]byte(`{"envelope":{},"events":[],"events":[]}`),
	} {
		want := canonicalVerificationOracle(candidate)
		for i := 0; i < 2; i++ { // both cold and previously visited inputs
			got, err := v.Verify(ctx, HashContent(candidate), candidate)
			if (err == nil) != (want == nil) {
				t.Fatalf("acceptance differs: %v / %v", err, want)
			}
			if err == nil && (!got.Valid() || !bytes.Equal(got.Bytes(), candidate)) {
				t.Fatal("identity changed")
			}
			if err != nil && got.Valid() {
				t.Fatal("failure returned a proof")
			}
		}
	}
	if _, err := v.Verify(ctx, HashContent([]byte("forged")), raw); !errors.Is(err, ErrIntegrity) {
		t.Fatal("claimed hash bypass", err)
	}
	verified, err := v.Verify(ctx, HashContent(raw), raw)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte{}, raw...)
	raw[0] = '!'
	copy := verified.Bytes()
	copy[0] = '?'
	if !bytes.Equal(verified.Bytes(), want) {
		t.Fatal("proof aliases input/output")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := v.Verify(canceled, HashContent(want), want); !errors.Is(err, context.Canceled) || got.Valid() {
		t.Fatal("canceled verification accepted", err)
	}
}

func TestCanonicalDocVerifierConcurrentAndBounded(t *testing.T) {
	var v CanonicalDocVerifier
	raw := verifierFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if _, err := v.Verify(context.Background(), HashContent(raw), raw); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	// More independently valid events than capacity cannot grow retained state
	// indefinitely. An evicted earlier document remains fully verifiable.
	cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "1"}}
	for i := 0; i < maxCanonicalEventProofs+3; i++ {
		cir.Events = append(cir.Events, CIREvent{Kind: EventMessage, Role: RoleUser, Seq: i})
	}
	large, err := CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), HashContent(large), large); err != nil {
		t.Fatal(err)
	}
	if len(v.events) != maxCanonicalEventProofs || len(v.order) != maxCanonicalEventProofs {
		t.Fatal("unbounded event proofs")
	}
	if _, err := v.Verify(context.Background(), HashContent(raw), raw); err != nil {
		t.Fatal("eviction changed acceptance", err)
	}
}

func FuzzCanonicalDocVerifierParity(f *testing.F) {
	f.Add(string(verifierFixture(f)))
	f.Add(`{"envelope":{"cir_version":"1"},"events":[]}`)
	f.Add(`{"envelope":null,"events":null}`)
	f.Fuzz(func(t *testing.T, text string) {
		raw := []byte(text)
		var v CanonicalDocVerifier
		// Prime with the normalized version, then challenge with the original.
		var cir CIRDocument
		if json.Unmarshal(raw, &cir) == nil {
			if canonical, err := CanonicalBytes(cir); err == nil {
				if _, err := v.Verify(context.Background(), HashContent(canonical), canonical); err != nil {
					t.Fatal("rejected canonical output", err)
				}
			}
		}
		_, got := v.Verify(context.Background(), HashContent(raw), raw)
		want := canonicalVerificationOracle(raw)
		if (got == nil) != (want == nil) {
			t.Fatalf("acceptance differs: got %v want %v", got, want)
		}
	})
}

func BenchmarkCanonicalDocVerification(b *testing.B) {
	cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2"}}
	for i := 0; i < 256; i++ {
		cir.Events = append(cir.Events, CIREvent{Kind: EventMessage, Seq: i, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: strings.Repeat("synthetic context ", 4096)}}})
	}
	raw, err := CanonicalBytes(cir)
	if err != nil {
		b.Fatal(err)
	}
	for _, variant := range []string{"typed", "cold", "warm-append"} {
		b.Run(variant, func(b *testing.B) {
			var v CanonicalDocVerifier
			if variant == "warm-append" {
				if _, err := v.Verify(context.Background(), HashContent(raw), raw); err != nil {
					b.Fatal(err)
				}
				cir.Events = append(cir.Events, CIREvent{Kind: EventMessage, Seq: len(cir.Events), Role: RoleAssistant})
			}
			body, err := CanonicalBytes(cir)
			if err != nil {
				b.Fatal(err)
			}
			hash := HashContent(body)
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if variant == "typed" {
					err = canonicalVerificationOracle(body)
				} else if variant == "cold" {
					fresh := CanonicalDocVerifier{}
					_, err = fresh.Verify(context.Background(), hash, body)
				} else {
					_, err = v.Verify(context.Background(), hash, body)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
