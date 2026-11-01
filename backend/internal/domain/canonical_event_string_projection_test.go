package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func giantCanonicalString(t testing.TB, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func giantCanonicalDocument(t testing.TB, event string) []byte {
	t.Helper()
	envelope, err := canonicalJSON(CIREnvelope{CIRVersion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	return []byte(canonicalDocPrefix + string(envelope) + canonicalDocMiddle + event + canonicalDocSuffix)
}

func TestCanonicalGiantStringProjectionOracleParity(t *testing.T) {
	large := giantCanonicalString(t, strings.Repeat("context 한글 <>&\n\t\\\"\u2028\x00", 6000))
	largeKey := giantCanonicalString(t, strings.Repeat("key", canonicalCheckBytes))
	cases := map[string]string{
		"message":              `{"blocks":[{"text":` + large + `,"type":"text"}],"kind":"message","role":"user","seq":1}`,
		"role":                 `{"kind":"turn","role":` + large + `,"seq":1}`,
		"content_type":         `{"blocks":[{"text":"body","type":` + large + `}],"kind":"message","role":"user","seq":1}`,
		"identity":             `{"id":` + large + `,"kind":"turn","role":"user","seq":1,"ts":` + large + `}`,
		"tool_call":            `{"call_id":` + large + `,"input":{"a":[` + large + `],"z":` + large + `},"kind":"tool_call","provider_tool_name":` + large + `,"seq":1,"status":` + large + `,"tool_name":` + large + `}`,
		"tool_result":          `{"call_id":"tool","kind":"tool_result","output":` + large + `,"seq":1}`,
		"tool_result_nested":   `{"call_id":"tool","kind":"tool_result","output":{"a":[{"value":` + large + `}],"z":false},"seq":1}`,
		"locked":               `{"cross_replayable":false,"kind":"reasoning","locked":{"blob":` + large + `,"provider":` + large + `,"scheme":` + large + `},"redacted_summary":` + large + `,"seq":1}`,
		"agent":                `{"agent_author":` + large + `,"agent_message":true,"agent_recipient":` + large + `,"blocks":[],"kind":"message","role":"assistant","seq":1}`,
		"metadata":             `{"kind":"turn","provider_metadata":{"create_time":1e9999,"turn_id":` + large + `},"role":"user","seq":1}`,
		"replacement":          `{"kind":"compaction","replacement":[{"blocks":[{"text":` + large + `,"type":"text"}],"kind":"message","role":"user","seq":1}],"replacement_complete":false,"seq":2}`,
		"key":                  `{"call_id":"tool","kind":"tool_result","output":{` + largeKey + `:` + large + `},"seq":1}`,
		"unknown_kind":         `{"kind":` + large + `,"seq":1}`,
		"unknown_field":        `{"kind":"turn","role":"user","seq":1,"unknown":` + large + `}`,
		"duplicate_field":      `{"call_id":"tool","kind":"tool_result","output":` + large + `,"output":"x","seq":1}`,
		"wrong_union":          `{"agent_author":` + large + `,"kind":"turn","role":"user","seq":1}`,
		"wrong_type":           `{"kind":"turn","role":"user","seq":` + large + `}`,
		"number_string":        `{"kind":"turn","provider_metadata":{"create_time":` + giantCanonicalString(t, "1"+strings.Repeat("0", canonicalCheckBytes)) + `},"role":"user","seq":1}`,
		"unsorted_replacement": `{"kind":"compaction","replacement":[{"kind":"turn","role":` + large + `,"seq":2},{"kind":"turn","role":"user","seq":1}],"replacement_complete":true,"seq":3}`,
		"null_metadata":        `{"kind":"turn","provider_metadata":null,"role":` + large + `,"seq":1}`,
		"null_lock":            `{"cross_replayable":false,"kind":"reasoning","locked":null,"redacted_summary":` + large + `,"seq":1}`,
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			raw := giantCanonicalDocument(t, event)
			want := canonicalVerificationOracle(raw)
			_, archivedWant := archivedColdCanonicalVerify(context.Background(), HashContent(raw), raw)
			original := bytes.Clone(raw)
			var verifier CanonicalDocVerifier
			for attempt := 0; attempt < 2; attempt++ {
				proof, err := verifier.Verify(context.Background(), HashContent(raw), raw)
				if (err == nil) != (want == nil) || (err != nil && proof.Valid()) {
					t.Fatalf("acceptance differs: got %v, oracle %v", err, want)
				}
				if errors.Is(err, ErrUnsupportedCIRVersion) != errors.Is(archivedWant, ErrUnsupportedCIRVersion) {
					t.Fatalf("version error differs: got %v, archived %v", err, archivedWant)
				}
				if err == nil && (!proof.Valid() || !bytes.Equal(proof.Bytes(), raw)) {
					t.Fatal("projected verification changed the original proof")
				}
			}
			if !bytes.Equal(raw, original) {
				t.Fatal("verification mutated the source")
			}
			legacy := bytes.Replace(raw, []byte(`"cir_version":"2"`), []byte(`"cir_version":"1"`), 1)
			_, err := verifier.Verify(context.Background(), HashContent(legacy), legacy)
			_, legacyWant := archivedColdCanonicalVerify(context.Background(), HashContent(legacy), legacy)
			if (err == nil) != (canonicalVerificationOracle(legacy) == nil) || errors.Is(err, ErrUnsupportedCIRVersion) != errors.Is(legacyWant, ErrUnsupportedCIRVersion) {
				t.Fatal("version/presence oracle changed", err)
			}
		})
	}
}

func TestCanonicalGiantStringProjectionErrorPrecedence(t *testing.T) {
	large := strings.Repeat("a", canonicalCheckBytes+1)
	number := giantCanonicalString(t, "1"+strings.Repeat("0", canonicalCheckBytes))
	for _, event := range []string{
		`{"agent_message":false,"blocks":[{"text":"` + large + `<","type":"text"}],"kind":"message","role":"user","seq":0}`,
		`{"agent_message":false,"blocks":[{"text":"` + large + `\q","type":"text"}],"kind":"message","role":"user","seq":0}`,
		`{"blocks":[],"kind":"message","provider_metadata":{"create_time":` + number + `},"role":"user","seq":0}`,
		`{"blocks":[],"kind":"message","provider_metadata":{"CREATE_TIME" : ` + number + `},"role":"user","seq":0}`,
		`{"blocks":[],"kind":"message","provider_metadata":{"\u0063reate_time":` + number + `},"role":"user","seq":0}`,
		`{"call_id":"tool","kind":"tool_result","output":"` + large + `","provider_metadata":{"create_time":` + number + `},"seq":0}`,
	} {
		raw := bytes.Replace(giantCanonicalDocument(t, event), []byte(`"cir_version":"2"`), []byte(`"cir_version":"1"`), 1)
		_, want := archivedColdCanonicalVerify(context.Background(), HashContent(raw), raw)
		var verifier CanonicalDocVerifier
		proof, err := verifier.Verify(context.Background(), HashContent(raw), raw)
		if want == nil || err == nil || proof.Valid() || len(verifier.events) != 0 ||
			errors.Is(err, ErrUnsupportedCIRVersion) != errors.Is(want, ErrUnsupportedCIRVersion) {
			t.Fatalf("error precedence changed: got %v, oracle %v", err, want)
		}
	}
}

func TestCanonicalGiantStringFragmentBoundaries(t *testing.T) {
	for shift := -12; shift <= 12; shift++ {
		for _, value := range []string{"한글😀", "\x00<>&\u2028\u2029", "\\\"\n\r\t\b\f"} {
			text := strings.Repeat("a", canonicalCheckBytes+shift) + value + strings.Repeat("b", canonicalCheckBytes) + value
			quoted := []byte(giantCanonicalString(t, text))
			if err := verifyCanonicalStringFragments(context.Background(), quoted[1:len(quoted)-1]); err != nil {
				t.Fatalf("shift %d/value %q: %v", shift, value, err)
			}
		}
	}
	for name, suffix := range map[string]string{
		"html": "<", "slash": `\/`, "unicode": `\u0061`, "surrogate": `\ud83d\ude00`,
		"upper_hex": `\u003C`, "bad_escape": `\q`, "short_unicode": `\u00`,
		"bad_hex": `\uZZZZ`, "raw_control": "\n", "raw_separator": "\u2028",
		"bad_utf8": string([]byte{0xff}), "truncated_utf8": string([]byte{0xf0, 0x9f}),
		"orphan_continuations": strings.Repeat(string([]byte{0x80}), canonicalCheckBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			content := []byte(strings.Repeat("x", canonicalCheckBytes-9) + suffix + strings.Repeat("y", canonicalCheckBytes))
			if err := verifyCanonicalStringFragments(context.Background(), content); !errors.Is(err, ErrIntegrity) {
				t.Fatal("accepted noncanonical string", err)
			}
			raw := giantCanonicalDocument(t, `{"call_id":"tool","kind":"tool_result","output":"`+string(content)+`","seq":1}`)
			var verifier CanonicalDocVerifier
			_, err := verifier.Verify(context.Background(), HashContent(raw), raw)
			if err == nil || canonicalVerificationOracle(raw) == nil || len(verifier.events) != 0 {
				t.Fatal("invalid string produced a proof or changed oracle acceptance", err)
			}
		})
	}
}

func TestCanonicalGiantStringCancellationNeverAdmits(t *testing.T) {
	value := giantCanonicalString(t, strings.Repeat("a", 4*canonicalCheckBytes))
	raw := giantCanonicalDocument(t, `{"call_id":"tool","kind":"tool_result","output":`+value+`,"seq":1}`)
	hash := HashContent(raw)
	checks := 0
	observed := canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}
	var countVerifier CanonicalDocVerifier
	if _, err := countVerifier.Verify(observed, hash, raw); err != nil {
		t.Fatal(err)
	}
	if checks < 20 {
		t.Fatal("giant verification did not expose bounded checkpoints", checks)
	}
	for stop := 1; stop <= checks; stop++ {
		base, cancel := context.WithCancel(context.Background())
		calls := 0
		ctx := canonicalAdmissionContext{Context: base, check: func() {
			calls++
			if calls == stop {
				cancel()
			}
		}}
		var verifier CanonicalDocVerifier
		proof, err := verifier.Verify(ctx, hash, raw)
		cancel()
		if !errors.Is(err, context.Canceled) || proof.Valid() || len(verifier.events) != 0 || len(verifier.order) != 0 {
			t.Fatalf("checkpoint %d/%d accepted incomplete verification: %v", stop, checks, err)
		}
	}
	content := []byte(strings.Repeat("x", 4*canonicalCheckBytes) + `\q`)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	ctx := canonicalAdmissionContext{Context: base, check: func() {
		calls++
		if calls == 5 {
			cancel()
		}
	}}
	if err := verifyCanonicalStringFragments(ctx, content); !errors.Is(err, context.Canceled) {
		t.Fatal("fragment processing did not stop before malformed tail", err)
	}
	if err := verifyCanonicalStringFragments(context.Background(), content); !errors.Is(err, ErrIntegrity) {
		t.Fatal("uncanceled control did not reach malformed tail", err)
	}
}

func TestCanonicalVerificationFrameMalformedParity(t *testing.T) {
	valid := giantCanonicalDocument(t, `{"kind":"turn","role":"user","seq":1}`)
	for _, raw := range [][]byte{
		valid, []byte(`{"envelope":]}`), []byte(`{"envelope":{},"events":[]}`),
		bytes.Replace(valid, []byte(`"events":[`), []byte(`"events":[,`), 1),
		bytes.Replace(valid, []byte(`"events":[`), []byte(`"events":[true,`), 1),
		bytes.Replace(valid, []byte(`"seq":1`), []byte(`"seq":1,`), 1),
		bytes.Replace(valid, []byte(`"seq":1`), []byte(`"seq":01`), 1),
		bytes.Replace(valid, []byte(`"seq":1`), []byte(`"seq":1 "id":"x"`), 1),
		bytes.Replace(valid, []byte(`"envelope":{`), []byte(`"envelope":[`), 1),
		bytes.Replace(valid, []byte(`"events":[`), []byte(`"events": [ `), 1),
		bytes.Replace(valid, []byte(`"events":[`), []byte(`"events":[ `), 1),
		append(bytes.Clone(valid), '\n'),
	} {
		var verifier CanonicalDocVerifier
		_, err := verifier.Verify(context.Background(), HashContent(raw), raw)
		if (err == nil) != (canonicalVerificationOracle(raw) == nil) {
			t.Fatalf("framing acceptance changed for %q: %v", raw, err)
		}
	}
}

func TestCanonicalVerificationFrameVersionErrorPrecedence(t *testing.T) {
	valid := giantCanonicalDocument(t, `{"agent_message":false,"blocks":[],"kind":"message","role":"user","seq":1}`)
	for _, version := range []string{"1", "99"} {
		raw := bytes.Replace(valid, []byte(`"cir_version":"2"`), []byte(`"cir_version":"`+version+`"`), 1)
		for _, candidate := range [][]byte{
			raw,
			bytes.Replace(raw, []byte(`"events":[`), []byte(`"events":[ `), 1),
			bytes.Replace(raw, []byte(`"seq":1}]`), []byte(`"seq":1},{"kind":}]`), 1),
			bytes.Replace(raw, []byte(`"seq":1}]`), []byte(`"seq":1},{"kind":"turn","role":"user","seq":2,}]`), 1),
			bytes.Replace(raw, []byte(`"seq":1}]`), []byte(`"seq":1},true]`), 1),
		} {
			_, want := archivedColdCanonicalVerify(context.Background(), HashContent(candidate), candidate)
			var verifier CanonicalDocVerifier
			proof, err := verifier.Verify(context.Background(), HashContent(candidate), candidate)
			if (err == nil) != (want == nil) || proof.Valid() ||
				errors.Is(err, ErrUnsupportedCIRVersion) != errors.Is(want, ErrUnsupportedCIRVersion) {
				t.Fatalf("outer/version error changed: got %v, archived %v for %q", err, want, candidate)
			}
		}
	}
}

func FuzzCanonicalGiantStringProjectionParity(f *testing.F) {
	for _, seed := range []string{"plain", "한글😀<>&\u2028\\\"", "\x00\n\t", string([]byte{0xff})} {
		f.Add(seed, uint8(0), uint16(0))
	}
	f.Fuzz(func(t *testing.T, text string, mutation uint8, position uint16) {
		if len(text) > 4096 {
			return
		}
		value := giantCanonicalString(t, strings.Repeat("x", canonicalCheckBytes)+text)
		raw := giantCanonicalDocument(t, `{"call_id":"tool","kind":"tool_result","output":`+value+`,"seq":1}`)
		if mutation != 0 {
			raw[int(position)%len(raw)] = mutation
		}
		var verifier CanonicalDocVerifier
		_, err := verifier.Verify(context.Background(), HashContent(raw), raw)
		if (err == nil) != (canonicalVerificationOracle(raw) == nil) {
			t.Fatalf("giant string acceptance differs: %v", err)
		}
	})
}

func archivedColdCanonicalVerify(ctx context.Context, want ContentHash, raw []byte) (VerifiedSessionDoc, error) {
	if err := ctx.Err(); err != nil {
		return VerifiedSessionDoc{}, err
	}
	if ValidateContentHash(want) != nil || HashContent(raw) != want {
		return VerifiedSessionDoc{}, ErrIntegrity
	}
	envelopeRaw, stream, ok := canonicalStream(raw)
	if !ok {
		return VerifiedSessionDoc{}, ErrIntegrity
	}
	envelope, err := decodeCanonicalCIREnvelope(envelopeRaw)
	if err != nil {
		return VerifiedSessionDoc{}, err
	}
	previous, havePrevious := 0, false
	var pending []canonicalEventProof
	err = visitCanonicalEvents(ctx, stream, func(body []byte) error {
		key := canonicalEventKey{HashContent(body), envelope.CIRVersion}
		event, err := decodeCanonicalEvent(body)
		if err != nil {
			return ErrIntegrity
		}
		if err := ValidateCIRVersion(CIRDocument{Envelope: envelope, Events: []CIREvent{event}}); err != nil {
			return err
		}
		if event.Replacement != nil {
			event.Replacement = canonicalEvents(event.Replacement)
		}
		canonical, err := canonicalDecodedEventJSON(event)
		if err != nil || !bytes.Equal(canonical, body) || (havePrevious && event.Seq < previous) {
			return ErrIntegrity
		}
		previous, havePrevious = event.Seq, true
		if len(pending) < maxCanonicalEventProofs {
			pending = append(pending, canonicalEventProof{key, event.Seq})
		}
		return ctx.Err()
	})
	if err != nil {
		return VerifiedSessionDoc{}, err
	}
	var verifier CanonicalDocVerifier
	verifier.remember(pending)
	return VerifiedSessionDoc{hash: want, canonical: string(raw)}, ctx.Err()
}

func BenchmarkCanonicalGiantStringColdVerification(b *testing.B) {
	for _, size := range []int{1 << 20, 8 << 20, 24 << 20} {
		value := giantCanonicalString(b, strings.Repeat("x", size))
		raw := giantCanonicalDocument(b, `{"call_id":"tool","kind":"tool_result","output":`+value+`,"seq":1}`)
		want := HashContent(raw)
		for _, variant := range []string{"archived", "projected"} {
			b.Run(fmt.Sprintf("bytes=%d/%s", size, variant), func(b *testing.B) {
				runtime.GC()
				runtime.GC()
				b.ReportAllocs()
				b.SetBytes(int64(len(raw)))
				b.ResetTimer()
				for iteration := 0; iteration < b.N; iteration++ {
					var proof VerifiedSessionDoc
					var err error
					if variant == "archived" {
						proof, err = archivedColdCanonicalVerify(context.Background(), want, raw)
					} else {
						var verifier CanonicalDocVerifier
						proof, err = verifier.Verify(context.Background(), want, raw)
					}
					if err != nil || !proof.Valid() {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
