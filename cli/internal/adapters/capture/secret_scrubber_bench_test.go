package capture_test

import (
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

var scrubLargeTextSink domain.CIRDocument

func BenchmarkScrubLargeText(b *testing.B) {
	for _, tier := range []capture.ScrubTier{capture.ScrubStandard, capture.ScrubStrict} {
		for _, scenario := range []string{"clean", "dense", "late"} {
			b.Run(string(tier)+"/"+scenario, func(b *testing.B) {
				line := "synthetic public context example 0123456789 abcdefghijklmnopqrstuvwxyz\n"
				if scenario == "dense" {
					line += "fake credential sample sk-" + strings.Repeat("z", 24) + "\n"
				}
				text := strings.Repeat(line, (1<<20)/len(line))
				if scenario == "late" {
					text += " fake credential sk-" + strings.Repeat("z", 24)
				}
				doc := domain.CIRDocument{Events: []domain.Event{{Kind: domain.EventMessage, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}}}
				b.ReportAllocs()
				b.SetBytes(int64(len(text)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					scrubLargeTextSink = capture.Scrub(doc, tier)
				}
			})
		}
	}
}
