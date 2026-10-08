package domain

import (
	"context"
	"encoding/json"
	"testing"
)

// Isolate projection cost after the same verified root proof and read plan.
// This is not an end-to-end storage or HTTP benchmark.
func BenchmarkReadPlanRootRanges(b *testing.B) {
	raw := canonicalAdmissionSizedDoc(b, 256, 8<<20)
	var cir CIRDocument
	if err := json.Unmarshal(raw, &cir); err != nil {
		b.Fatal(err)
	}
	manifest, bodies, err := ConversationManifestForCIR(cir)
	if err != nil {
		b.Fatal(err)
	}
	var verifier CanonicalDocVerifier
	doc, err := verifier.VerifyConversationManifestDoc(context.Background(), manifestProofHash(b, manifest), manifest, manifestProofLoad(bodies))
	if err != nil {
		b.Fatal(err)
	}
	plan, err := doc.PlanReadIndexContext(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	for _, mode := range []struct {
		name  string
		build func(context.Context) (DocReadIndex, error)
	}{
		{"metadata", plan.BuildMetadataContext},
		{"ranges", plan.BuildRangesContext},
	} {
		b.Run(mode.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				index, err := mode.build(context.Background())
				if err != nil || len(index.Events) != 256 {
					b.Fatal("invalid projection", err)
				}
			}
		})
	}
}
