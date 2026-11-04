package domain

import (
	"context"
	"strings"
	"testing"
)

func BenchmarkLegacyVerifiedHeaderPreparation(benchmark *testing.B) {
	for _, fixture := range []struct {
		name       string
		headerSize int
		bodySize   int
	}{
		{name: "short-header", headerSize: 64},
		{name: "large-header", headerSize: 4*canonicalCheckBytes + 17},
		{name: "large-transcript", bodySize: 8 * ConversationManifestChunkBytes},
	} {
		benchmark.Run(fixture.name, func(benchmark *testing.B) {
			var proof VerifiedSessionDoc
			if fixture.bodySize != 0 {
				proof, _ = legacyDocByteBackfillSizedFixture(benchmark, fixture.bodySize)
			} else {
				proof, _ = legacyDocByteBackfillDocumentFixture(benchmark, CIRDocument{
					Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{strings.Repeat("h", fixture.headerSize)}},
					Events:   []CIREvent{},
				})
			}
			plan, err := NewLegacyByteBackfill(context.Background(), proof)
			if err != nil {
				benchmark.Fatal(err)
			}
			benchmark.Run("plan", func(benchmark *testing.B) {
				benchmark.ReportAllocs()
				for benchmark.Loop() {
					if next, err := NewLegacyByteBackfill(context.Background(), proof); err != nil || !next.Valid() {
						benchmark.Fatal(err)
					}
				}
			})
			benchmark.Run("seed", func(benchmark *testing.B) {
				benchmark.ReportAllocs()
				for benchmark.Loop() {
					if seed, err := NewLegacyRangeMetadataSeed(context.Background(), proof, plan); err != nil || seed.Reference() != proof.DocumentRef() {
						benchmark.Fatal(err)
					}
				}
			})
		})
	}
}
