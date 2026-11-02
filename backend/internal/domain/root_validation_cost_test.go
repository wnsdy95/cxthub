package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func strictCanonicalEventReference(ctx context.Context, version string, raw []byte) (int, error) {
	var event CIREvent
	if json.Unmarshal(raw, &event) != nil {
		return 0, ErrIntegrity
	}
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: version}, Events: []CIREvent{event}}
	if err := ValidateCIRVersion(document); err != nil {
		return 0, err
	}
	event = canonicalEvents(document.Events)[0]
	canonical, err := canonicalDecodedEventJSON(event)
	if err != nil || !bytes.Equal(canonical, raw) {
		return 0, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return event.Seq, nil
}

func TestConversationManifestEventStrictCacheParity(tests *testing.T) {
	cases := canonicalEventCases()
	cases = append(cases,
		canonicalEventCase{"empty_message", []byte(`{"blocks":[],"kind":"message","role":"user","seq":17}`)},
		canonicalEventCase{"agent_message", []byte(`{"agent_message":true,"blocks":[],"kind":"message","role":"assistant","seq":17}`)},
		canonicalEventCase{"present_false", []byte(`{"agent_message":false,"blocks":[],"kind":"message","role":"assistant","seq":17}`)},
		canonicalEventCase{"null_presence", []byte(`{"blocks":[],"kind":"message","locked":null,"role":"assistant","seq":17}`)},
		canonicalEventCase{"sequence_precision", []byte(`{"kind":"turn","role":"user","seq":9007199254740993}`)},
		canonicalEventCase{"payload_precision", []byte(`{"call_id":"c","input":{"number":9007199254740993},"kind":"tool_call","seq":17,"tool_name":"tool"}`)},
		canonicalEventCase{"compaction_empty", []byte(`{"kind":"compaction","replacement":[],"replacement_complete":true,"seq":17}`)},
		canonicalEventCase{"compaction_unsorted", []byte(`{"kind":"compaction","replacement":[{"kind":"turn","role":"user","seq":3},{"kind":"turn","role":"assistant","seq":1}],"replacement_complete":true,"seq":17}`)},
		canonicalEventCase{"compaction_equal", []byte(`{"kind":"compaction","replacement":[{"kind":"turn","role":"user","seq":3},{"kind":"turn","role":"assistant","seq":3}],"replacement_complete":true,"seq":17}`)},
	)
	for _, version := range []string{"", CIRVersionV1, CIRVersionV2, "99"} {
		tests.Run(fmt.Sprintf("version=%q", version), func(test *testing.T) {
			for _, candidate := range cases {
				test.Run(candidate.name, func(test *testing.T) {
					before := bytes.Clone(candidate.raw)
					for _, canceled := range []bool{false, true} {
						ctx, cancel := context.WithCancel(context.Background())
						if canceled {
							cancel()
						}
						var verifier CanonicalDocVerifier
						var pending []canonicalEventProof
						want, expectedError := strictCanonicalEventReference(ctx, version, candidate.raw)
						cached, cachedError := verifier.event(ctx, version, candidate.raw, &pending)
						got, actualError := conversationManifestEvent(ctx, version, candidate.raw)
						cancel()
						if got != want || cached != want || fmt.Sprint(cachedError) != fmt.Sprint(expectedError) || fmt.Sprint(actualError) != fmt.Sprint(expectedError) ||
							errors.Is(actualError, context.Canceled) != errors.Is(expectedError, context.Canceled) ||
							errors.Is(actualError, ErrUnsupportedCIRVersion) != errors.Is(expectedError, ErrUnsupportedCIRVersion) {
							test.Fatal("cache-free event validation changed strict semantics", canceled, got, want, actualError, expectedError)
						}
						if !bytes.Equal(before, candidate.raw) {
							test.Fatal("event validation mutated its source bytes")
						}
					}
				})
			}
		})
	}
}

func FuzzPureCanonicalEventParity(fuzzer *testing.F) {
	for _, candidate := range canonicalEventCases() {
		if len(candidate.raw) <= 16<<10 {
			fuzzer.Add(candidate.raw)
		}
	}
	fuzzer.Fuzz(func(test *testing.T, raw []byte) {
		if len(raw) > 16<<10 {
			test.Skip()
		}
		var reference CIREvent
		referenceError := json.Unmarshal(raw, &reference)
		candidate, candidateError := decodeCanonicalEvent(raw)
		if (referenceError == nil) != (candidateError == nil) || referenceError == nil && !reflect.DeepEqual(reference, candidate) {
			test.Fatal("private event decoding changed field values or exact key presence", candidateError, referenceError)
		}
		for _, version := range []string{"", CIRVersionV1, CIRVersionV2, "99"} {
			want, expectedError := strictCanonicalEventReference(context.Background(), version, raw)
			got, actualError := conversationManifestEvent(context.Background(), version, raw)
			if got != want || fmt.Sprint(actualError) != fmt.Sprint(expectedError) {
				test.Fatal("pure strict validator differs from predecessor", version, got, want, actualError, expectedError)
			}
		}
	})
}

func TestCanonicalEventDecodePresenceParity(tests *testing.T) {
	for _, candidate := range canonicalEventCases() {
		tests.Run(candidate.name, func(test *testing.T) {
			var reference CIREvent
			expectedError := json.Unmarshal(candidate.raw, &reference)
			actual, actualError := decodeCanonicalEvent(candidate.raw)
			if (expectedError == nil) != (actualError == nil) || expectedError == nil && !reflect.DeepEqual(reference, actual) {
				test.Fatal("private event decoder changed public field presence or values", actual, reference, actualError, expectedError)
			}
		})
	}
}

func BenchmarkCanonicalEventDecode(benchmarks *testing.B) {
	for _, size := range []int{128, 1 << 20} {
		event := CIREvent{Kind: EventMessage, Seq: 17, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: strings.Repeat("x", size)}}}
		raw, err := canonicalJSON(event)
		if err != nil {
			benchmarks.Fatal(err)
		}
		for _, private := range []bool{false, true} {
			benchmarks.Run(fmt.Sprintf("bytes=%d/private=%t", size, private), func(benchmark *testing.B) {
				benchmark.ReportAllocs()
				benchmark.SetBytes(int64(len(raw)))
				for benchmark.Loop() {
					var decoded CIREvent
					var err error
					if private {
						decoded, err = decodeCanonicalEvent(raw)
					} else {
						err = json.Unmarshal(raw, &decoded)
					}
					if err != nil || decoded.Seq != 17 || decoded.Blocks[0].Text != event.Blocks[0].Text {
						benchmark.Fatal("event decode failed", err)
					}
				}
			})
		}
	}
}

func BenchmarkRootManifestValidationCold(benchmarks *testing.B) {
	for _, count := range []int{12800, 128000} {
		benchmarks.Run(fmt.Sprint(count), func(benchmark *testing.B) {
			document := CIRDocument{Envelope: CIREnvelope{CIRVersion: CIRVersionV1, SourceProvider: ProviderCodex, Fidelity: FidelityFull}}
			for ordinal := 0; ordinal < count; ordinal++ {
				document.Events = append(document.Events, CIREvent{Kind: EventMessage, Seq: ordinal + 1, Role: RoleUser,
					Blocks: []ContentBlock{{Type: "text", Text: fmt.Sprintf("root validation message %d", ordinal)}}})
			}
			manifest, bodies, err := ConversationManifestForCIR(document)
			if err != nil {
				benchmark.Fatal(err)
			}
			hash, err := ConversationManifestHash(manifest)
			if err != nil {
				benchmark.Fatal(err)
			}
			load := func(ctx context.Context, selected ContentHash) ([]byte, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				body, found := bodies[selected]
				if !found {
					return nil, ErrNotFound
				}
				return body, nil
			}
			benchmark.ReportAllocs()
			benchmark.SetBytes(manifest.StreamBytes)
			benchmark.ResetTimer()
			for benchmark.Loop() {
				if err := VerifyConversationManifest(context.Background(), hash, manifest, load); err != nil {
					benchmark.Fatal(err)
				}
			}
		})
	}
}
