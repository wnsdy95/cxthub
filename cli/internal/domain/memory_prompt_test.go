package domain

import (
	"reflect"
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

func TestPromptStructuredProjectionKeepsOnlyLatestAuthoritativeTaskNarrative(t *testing.T) {
	ids := []ContentHash{
		ContentHash("sha256:" + strings.Repeat("a", 64)),
		ContentHash("sha256:" + strings.Repeat("b", 64)),
		ContentHash("sha256:" + strings.Repeat("c", 64)),
	}
	oldSummary := providerContinuation + `

Summary:
1. Primary Request and Intent:
   Keep old project history.
7. Pending Tasks:
   - Superseded detailed task.
8. Current Work:
   Old implementation result remains useful.`
	unattestedSummary := providerContinuation + `

Summary:
1. Primary Request and Intent:
   Keep fallback project history.
7. Pending Tasks:
   - Extractive fallback must not become a command.
8. Current Work:
   Fallback implementation result remains useful.`
	currentSummary := providerContinuation + `

Summary:
1. Primary Request and Intent:
   Keep current project history.
7. Pending Tasks:
   - Current detailed task remains.
8. Current Work:
   Current implementation result remains useful.`
	digest := MemoryDigest{Fragments: []MemoryFragment{
		{SourceSnapshot: ids[0], Summary: oldSummary, OpenTasks: []string{"Superseded detailed task."}, TasksAuthoritative: true},
		{SourceSnapshot: ids[1], Summary: unattestedSummary, OpenTasks: []string{"Extractive fallback must not become a command."}},
		{SourceSnapshot: ids[2], Summary: currentSummary, OpenTasks: []string{"Current detailed task remains."}, TasksAuthoritative: true},
	}}
	digest = MergeDigests(MemoryDigest{}, digest)
	original := cloneMemoryDigestForTest(digest)

	got := PromptStructuredProjection(digest)
	for _, removed := range []string{"Superseded detailed task", "Extractive fallback must not become a command"} {
		if strings.Contains(got.Summary, removed) || containsString(got.OpenTasks, removed+".") {
			t.Fatalf("superseded/unattested task survived %q:\n%+v", removed, got)
		}
	}
	for _, kept := range []string{
		"Old implementation result remains useful.",
		"Fallback implementation result remains useful.",
		"Current detailed task remains.",
		"Current implementation result remains useful.",
	} {
		if !strings.Contains(got.Summary, kept) && !containsString(got.OpenTasks, kept) {
			t.Fatalf("prompt projection lost %q:\n%+v", kept, got)
		}
	}
	if !reflect.DeepEqual(got.OpenTasks, []string{"Current detailed task remains."}) {
		t.Fatalf("current task projection = %v", got.OpenTasks)
	}
	if !reflect.DeepEqual(digest, original) {
		t.Fatal("narrative projection mutated immutable digest")
	}
}

func TestPromptStructuredProjectionDropsOnlyByteContainedProviderBaseline(t *testing.T) {
	leftID := ContentHash("sha256:" + strings.Repeat("d", 64))
	rightID := ContentHash("sha256:" + strings.Repeat("e", 64))
	base := providerContinuation + `

Summary:
1. Primary Request and Intent:
   Shared baseline remains exactly represented.`
	expanded := base + `
8. Current Work:
   Newer exact extension remains.`
	digest := MemoryDigest{Fragments: []MemoryFragment{
		{SourceSnapshot: leftID, Summary: base},
		{SourceSnapshot: rightID, Summary: expanded},
	}}
	digest = MergeDigests(MemoryDigest{}, digest)

	got := PromptStructuredProjection(digest)
	if strings.Count(got.Summary, providerContinuation) != 1 ||
		strings.Count(got.Summary, "Shared baseline remains exactly represented.") != 1 ||
		!strings.Contains(got.Summary, "Newer exact extension remains.") {
		t.Fatalf("contained provider baseline was not losslessly collapsed:\n%s", got.Summary)
	}

	unique := MemoryDigest{Fragments: []MemoryFragment{
		{SourceSnapshot: leftID, Summary: providerContinuation + "\n\nSummary:\n1. Left-only decision."},
		{SourceSnapshot: rightID, Summary: providerContinuation + "\n\nSummary:\n1. Right-only decision."},
	}}
	unique = MergeDigests(MemoryDigest{}, unique)
	projected := PromptStructuredProjection(unique)
	for _, want := range []string{"Left-only decision.", "Right-only decision."} {
		if !strings.Contains(projected.Summary, want) {
			t.Fatalf("unique sibling summary lost %q:\n%s", want, projected.Summary)
		}
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
