package domain

import (
	"strings"
	"testing"
)

const providerContinuation = "This session is being continued from a previous conversation."

func TestLatestProviderCompactionGenerationKeepsOnlyNewestCumulativeBlock(t *testing.T) {
	old := providerContinuation + `

Summary:
1. Primary Request and Intent:
   Old request.
7. Pending Tasks:
   - Stale task.`
	latest := "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation." + `

Summary:
1. Primary Request and Intent:
   Current request.
7. Pending Tasks:
   - Current task.
8. Current Work:
   Current implementation state.`
	cumulative := "[... earlier summary omitted ...]\n" + old + "\n\n" + old + "\n\n" + latest

	got := LatestProviderCompactionGeneration(cumulative)
	if got != latest {
		t.Fatalf("latest generation mismatch:\n%s", got)
	}
	if strings.Contains(got, "Stale task") || strings.Count(got, "This session is being continued") != 1 {
		t.Fatalf("cumulative generations survived:\n%s", got)
	}

	single := "Provider prose mentioning the continuation format.\n" + providerContinuation
	if got := LatestProviderCompactionGeneration(single); got != single {
		t.Fatalf("single provider block changed: got %q want %q", got, single)
	}
}

func TestPromptStructuredProjectionKeepsConversationDeltaAfterStrippedTaskSection(t *testing.T) {
	base := providerContinuation + `

Summary:
1. Primary Request and Intent:
   Project baseline remains.
7. Pending Tasks:
   - Unattested task is removed.`
	summary := AppendExtractiveConversationDelta(base, RenderExtractiveFallbackSummary(
		[]string{"Recent user decision remains."},
		[]string{"Recent assistant result remains."},
	))
	digest := MemoryDigest{Summary: summary}

	got := PromptStructuredProjection(digest)
	if strings.Contains(got.Summary, "Unattested task is removed") {
		t.Fatalf("unattested task narrative survived:\n%s", got.Summary)
	}
	for _, want := range []string{
		"Project baseline remains.", "Recent user decision remains.", "Recent assistant result remains.",
	} {
		if !strings.Contains(got.Summary, want) {
			t.Fatalf("task stripping lost %q:\n%s", want, got.Summary)
		}
	}
}

func TestPromptStructuredProjectionDoesNotTruncateOpaqueLegacyNarrative(t *testing.T) {
	left := providerContinuation + `

Summary:
1. Primary Request and Intent:
   Unique left lineage narrative.`
	right := providerContinuation + `

Summary:
1. Primary Request and Intent:
   Unique right lineage narrative.`
	legacy := MemoryDigest{Summary: left + "\n\n" + right}

	got := PromptStructuredProjection(legacy)
	for _, want := range []string{"Unique left lineage narrative.", "Unique right lineage narrative."} {
		if !strings.Contains(got.Summary, want) {
			t.Fatalf("opaque legacy projection lost %q:\n%s", want, got.Summary)
		}
	}
}

func TestPromptProjectionDoesNotCarryPreviousCodeAssessment(t *testing.T) {
	block := CodeAssessmentBegin + "\nOLD CODE STATE: applied\n" + CodeAssessmentEnd
	d := MemoryDigest{Summary: "original history\n" + block + "\nrecent decision"}
	original := d.Summary
	out := HistoricalPromptProjection(d)
	if strings.Contains(out.Summary, "OLD CODE STATE") || !strings.Contains(out.Summary, "recent decision") || !strings.Contains(out.Summary, "original history") {
		t.Fatal(out.Summary)
	}
	if d.Summary != original {
		t.Fatal("archive mutated")
	}
	if got := WithoutCodeAssessment("unfinished " + CodeAssessmentBegin + " user text"); !strings.Contains(got, "user text") {
		t.Fatal("unclosed marker deleted history")
	}
}
