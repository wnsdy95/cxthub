package domain

import (
	"fmt"
	"strings"
	"testing"
)

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
			if err != nil {
				benchmark.Fatal(err)
			}
			benchmark.ReportAllocs()
			benchmark.SetBytes(int64(len(canonical)))
			benchmark.ResetTimer()
			for benchmark.Loop() {
				if err != nil {
					benchmark.Fatal(err)
				}
			}
		})
	}
}
