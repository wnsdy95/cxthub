package domain

import (
	"reflect"
	"strings"
	"testing"
)

const testProviderContinuation = "This session is being continued from a previous conversation."

func TestPromptStructuredProjectionKeepsOnlyLatestAttestedTasks(t *testing.T) {
	ids := []ContentHash{
		ContentHash("sha256:" + strings.Repeat("a", 64)),
		ContentHash("sha256:" + strings.Repeat("b", 64)),
		ContentHash("sha256:" + strings.Repeat("c", 64)),
	}
	old := testProviderContinuation + "\n\n1. Primary Request and Intent:\n   Old decision.\n7. Pending Tasks:\n   - Superseded task.\n8. Current Work:\n   Old result."
	fallback := testProviderContinuation + "\n\n1. Primary Request and Intent:\n   Fallback history.\n7. Pending Tasks:\n   - Unattested task.\n8. Current Work:\n   Fallback result."
	current := testProviderContinuation + "\n\n1. Primary Request and Intent:\n   Current decision.\n7. Pending Tasks:\n   - Current task.\n8. Current Work:\n   Current result."
	digest := MemoryDigest{Fragments: []MemoryFragment{
		{SourceSnapshot: ids[0], Summary: old, OpenTasks: []string{"Superseded task."}, TasksAuthoritative: true},
		{SourceSnapshot: ids[1], Summary: fallback, OpenTasks: []string{"Unattested task."}},
		{SourceSnapshot: ids[2], Summary: current, KeyFacts: []string{"Project fact remains.", "ingested from codex:rollout_summary"}, OpenTasks: []string{"Current task."}, TasksAuthoritative: true},
	}}
	original := cloneMemoryDigest(digest)

	got := PromptStructuredProjection(digest)
	for _, removed := range []string{"Superseded task", "Unattested task", "ingested from"} {
		if strings.Contains(got.Summary, removed) || containsString(got.OpenTasks, removed+".") || containsString(got.KeyFacts, removed) {
			t.Fatalf("unsafe prompt state survived %q: %+v", removed, got)
		}
	}
	for _, want := range []string{"Old result.", "Fallback result.", "Current result."} {
		if !strings.Contains(got.Summary, want) {
			t.Fatalf("project history lost %q:\n%s", want, got.Summary)
		}
	}
	if !reflect.DeepEqual(got.OpenTasks, []string{"Current task."}) || !reflect.DeepEqual(got.KeyFacts, []string{"Project fact remains."}) {
		t.Fatalf("structured projection = facts %v tasks %v", got.KeyFacts, got.OpenTasks)
	}
	if !reflect.DeepEqual(digest, original) {
		t.Fatal("projection mutated immutable digest")
	}
}

func TestPromptStructuredProjectionCollapsesOnlyCumulativeProviderGeneration(t *testing.T) {
	old := testProviderContinuation + "\n\n1. Primary Request and Intent:\n   Old duplicate."
	latest := "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\n1. Primary Request and Intent:\n   Latest state."
	got := LatestProviderCompactionGeneration(old + "\n\n" + old + "\n\n" + latest)
	if got != latest {
		t.Fatalf("latest generation = %q", got)
	}

	unique := MemoryDigest{Fragments: []MemoryFragment{
		{SourceSnapshot: ContentHash("sha256:" + strings.Repeat("d", 64)), Summary: testProviderContinuation + "\n\n1. Left-only decision."},
		{SourceSnapshot: ContentHash("sha256:" + strings.Repeat("e", 64)), Summary: testProviderContinuation + "\n\n1. Right-only decision."},
	}}
	projected := PromptStructuredProjection(unique)
	for _, want := range []string{"Left-only decision.", "Right-only decision."} {
		if !strings.Contains(projected.Summary, want) {
			t.Fatalf("unique sibling memory lost %q:\n%s", want, projected.Summary)
		}
	}
}

func TestPromptStructuredProjectionTreatsLegacyTasksAsUnattested(t *testing.T) {
	digest := MemoryDigest{
		Summary:   "1. Project state:\n   Stable.\n7. Open Tasks:\n   Legacy instruction.\n8. Result:\n   Preserved result.",
		KeyFacts:  []string{"Useful project fact."},
		OpenTasks: []string{"Legacy instruction."},
	}
	got := PromptStructuredProjection(digest)
	if len(got.OpenTasks) != 0 || strings.Contains(got.Summary, "Legacy instruction") {
		t.Fatalf("legacy task became an instruction: %+v", got)
	}
	if !strings.Contains(got.Summary, "Preserved result") {
		t.Fatalf("legacy narrative was over-truncated: %s", got.Summary)
	}
}

func cloneMemoryDigest(d MemoryDigest) MemoryDigest {
	out := d
	out.KeyFacts = append([]string(nil), d.KeyFacts...)
	out.OpenTasks = append([]string(nil), d.OpenTasks...)
	out.Fragments = make([]MemoryFragment, len(d.Fragments))
	for i, fragment := range d.Fragments {
		out.Fragments[i] = fragment
		out.Fragments[i].KeyFacts = append([]string(nil), fragment.KeyFacts...)
		out.Fragments[i].OpenTasks = append([]string(nil), fragment.OpenTasks...)
	}
	return out
}
