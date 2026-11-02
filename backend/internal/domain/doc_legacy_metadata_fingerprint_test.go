package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyRangeMetadataExactFingerprint(tests *testing.T) {
	expected := map[int]ContentHash{
		0:                        "sha256:72123b822c4cc36b12754bce044dbe02f0515f82e136ee7c10d026924e2359a6",
		1:                        "sha256:13ee85fe22110a8db0ce19503f99f6233aa67e63f42697df9e904de136b05213",
		DocRangePageEvents + 3:   "sha256:7183b077198a279d63d2fe37f183a7659caa328976c068efd0b2f15821f2b245",
		3*DocRangePageEvents + 1: "sha256:b02e57ab5da833d43dad55adb99b96968128693e24929d8222bcb61eae992a9a",
	}
	for _, count := range []int{0, 1, DocRangePageEvents + 3, 3*DocRangePageEvents + 1} {
		tests.Run(fmt.Sprint(count), func(test *testing.T) {
			proof, _ := legacyDocRangeGenerationFixture(test, count)
			generation, objects, err := BuildLegacyDocRangeGeneration(context.Background(), proof)
			if err != nil {
				test.Fatal(err)
			}
			descriptor, err := generation.Encode()
			if err != nil {
				test.Fatal(err)
			}
			fingerprint := struct {
				Descriptor json.RawMessage `json:"descriptor"`
				Objects    []ContentHash   `json:"objects"`
			}{Descriptor: descriptor, Objects: make([]ContentHash, 0, len(objects))}
			for _, object := range objects {
				if HashContent(object.Bytes) != object.Hash {
					test.Fatal("metadata object changed its declared identity")
				}
				fingerprint.Objects = append(fingerprint.Objects, object.Hash)
			}
			raw, err := json.Marshal(fingerprint)
			if err != nil {
				test.Fatal(err)
			}
			if actual := HashContent(raw); actual != expected[count] {
				test.Fatalf("count=%d generation/object-order fingerprint=%s want=%s", count, actual, expected[count])
			}
		})
	}
}

func FuzzLegacyRangeMetadataSequenceParity(fuzzer *testing.F) {
	for _, count := range []int{0, 3, 9} {
		_, raw := legacyDocRangeGenerationFixture(fuzzer, count)
		fuzzer.Add(raw)
	}
	implicitLegacy := CIRDocument{
		Envelope: CIREnvelope{SourceProvider: ProviderCodex, Fidelity: FidelityFull},
		Events: []CIREvent{{Kind: EventMessage, Seq: 17, Role: RoleUser,
			Blocks: []ContentBlock{{Type: "text", Text: "implicit v1 archive"}}}},
	}
	raw, err := CanonicalBytes(implicitLegacy)
	if err != nil {
		fuzzer.Fatal(err)
	}
	fuzzer.Add(raw)
	fuzzer.Fuzz(func(test *testing.T, raw []byte) {
		if len(raw) > 16<<10 {
			test.Skip()
		}
		var document CIRDocument
		if json.Unmarshal(raw, &document) != nil || len(document.Events) > 16 {
			return
		}
		canonical, err := CanonicalBytes(document)
		if err != nil {
			return
		}
		proof, err := VerifySessionDoc(SessionDoc{Hash: HashContent(canonical), CIR: document})
		if err != nil {
			return
		}
		ctx := context.Background()
		plan, err := proof.PlanReadIndexContext(ctx)
		if err != nil {
			test.Fatal(err)
		}
		reference, err := plan.BuildRangesContext(ctx)
		if err != nil {
			test.Fatal(err)
		}
		generation, objects, err := BuildLegacyDocRangeGeneration(ctx, proof)
		envelope, envelopeErr := canonicalJSON(reference.Envelope)
		if envelopeErr != nil {
			test.Fatal(envelopeErr)
		}
		if err != nil {
			test.Fatal("verified legacy metadata could not prepare its range", generation, len(objects), err)
		}
		if generation.Hash != proof.Hash() || !bytes.Equal(generation.Envelope, envelope) {
			test.Fatal("range generation changed its original source identity or envelope")
		}
		entries := legacyDocRangeGenerationPages(test, generation, objects)
		if len(entries) != len(reference.Events) {
			test.Fatal("single-pass metadata changed the event count")
		}
		rendered := 0
		for ordinal, location := range reference.Events {
			body, err := proof.EventStreamRange(ctx, location.Offset, location.Length)
			if err != nil {
				test.Fatal(err)
			}
			expected, err := docRangeEntry(location.Index, location.Offset, body, location.Seq)
			if err != nil {
				test.Fatal(err)
			}
			expected.RenderedBefore = rendered
			if !expected.EmptyReasoning {
				rendered++
			}
			if !reflect.DeepEqual(entries[ordinal], expected) {
				test.Fatal("single-pass metadata changed prior range/sequence projection", ordinal, entries[ordinal], expected)
			}
		}
	})
}

var legacyMetadataBenchmarkGeneration DocRangeGeneration
var legacyMetadataBenchmarkObjects []DocRangeObject

func BenchmarkLegacyRangeMetadataGeneration(benchmark *testing.B) {
	for _, size := range []int{4 << 20, 16 << 20} {
		benchmark.Run(fmt.Sprintf("%dMiB", size>>20), func(benchmark *testing.B) {
			document := CIRDocument{
				Envelope: CIREnvelope{CIRVersion: "2", SourceProvider: ProviderCodex, Fidelity: FidelityFull},
				Events: []CIREvent{
					{Kind: EventToolResult, Seq: 17, CallID: "large-response", Output: strings.Repeat("x", size)},
					{Kind: EventMessage, Seq: 29, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "latest prompt"}}},
				},
			}
			canonical, err := CanonicalBytes(document)
			if err != nil {
				benchmark.Fatal(err)
			}
			proof, err := VerifySessionDoc(SessionDoc{Hash: HashContent(canonical), CIR: document})
			if err != nil {
				benchmark.Fatal(err)
			}
			benchmark.ReportAllocs()
			benchmark.SetBytes(int64(len(canonical)))
			benchmark.ResetTimer()
			for benchmark.Loop() {
				generation, objects, err := BuildLegacyDocRangeGeneration(context.Background(), proof)
				if err != nil {
					benchmark.Fatal(err)
				}
				legacyMetadataBenchmarkGeneration, legacyMetadataBenchmarkObjects = generation, objects
			}
		})
	}
}
