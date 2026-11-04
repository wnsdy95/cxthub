//go:build postgres

package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

var legacyMonolithicRangeBenchmarkProof domain.VerifiedSessionDoc

func BenchmarkLegacyMonolithicRangeSource(benchmark *testing.B) {
	for _, size := range []int{4 << 20, 16 << 20} {
		benchmark.Run(fmt.Sprintf("%dMiB", size>>20), func(benchmark *testing.B) {
			document := domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull},
				Events: []domain.CIREvent{
					{Kind: domain.EventToolResult, Seq: 1, CallID: "large-response", Output: strings.Repeat("x", size)},
					{Kind: domain.EventMessage, Seq: 2, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "latest prompt"}}},
				},
			}
			raw, err := domain.CanonicalBytes(document)
			if err != nil {
				benchmark.Fatal(err)
			}
			hash := domain.HashContent(raw)
			ctx := context.Background()
			for _, mode := range []string{"legacy", "cold", "warm"} {
				benchmark.Run(mode, func(benchmark *testing.B) {
					storage := &PostgresStore{}
					if mode == "warm" {
						if _, err := storage.verifyLegacyMonolithicRangeSource(ctx, hash, raw); err != nil {
							benchmark.Fatal(err)
						}
					}
					benchmark.ReportAllocs()
					benchmark.SetBytes(int64(len(raw)))
					benchmark.ResetTimer()
					for benchmark.Loop() {
						var proof domain.VerifiedSessionDoc
						var err error
						if mode == "legacy" {
							proof, err = verifyLegacyDecodedMonolithicDoc(ctx, hash, raw)
						} else {
							if mode == "cold" {
								storage = &PostgresStore{}
							}
							proof, err = storage.verifyLegacyMonolithicRangeSource(ctx, hash, raw)
						}
						if err != nil || !proof.Valid() {
							benchmark.Fatal(err)
						}
						legacyMonolithicRangeBenchmarkProof = proof
					}
				})
			}
		})
	}
}
