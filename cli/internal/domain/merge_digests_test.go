package domain

import (
	"testing"
)

func TestRenderExtractiveFallbackSummaryPreservesWireFormat(t *testing.T) {
	want := "Conversation digest (extractive fallback; provider compaction summary unavailable):\n\n\n" +
		"Recent user intent:\n- Keep the existing hash contract.\n\n\n" +
		"Recent assistant outcomes:\n- Verified the canonical whitespace."
	got := RenderExtractiveFallbackSummary(
		[]string{"Keep the existing hash contract."},
		[]string{"Verified the canonical whitespace."},
	)
	if got != want {
		t.Fatalf("extractive fallback wire format changed:\n got %q\nwant %q", got, want)
	}
}
