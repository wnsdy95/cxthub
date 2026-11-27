package app

import (
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// TestBoundCarriedDigestKeepsNewestTail (#33 — bounded carry): the forward
// working set is capped at memoryCarryBudgetBytes keeping the newest tail;
// under-budget digests pass through unchanged.
func TestBoundCarriedDigestKeepsNewestTail(t *testing.T) {
	small := domain.MemoryDigest{Summary: "compact history", KeyFacts: []string{"f"}}
	if got := boundCarriedDigest(small); got.Summary != small.Summary {
		t.Fatalf("under-budget summary changed: %q", got.Summary)
	}

	big := domain.MemoryDigest{
		Summary:            "OLDEST-GENERATION-HEAD\n" + strings.Repeat("m", 2*memoryCarryBudgetBytes) + "\nNEWEST-GENERATION-TAIL",
		KeyFacts:           []string{"fact stays"},
		OpenTasks:          []string{"task stays"},
		TasksAuthoritative: true,
	}
	got := boundCarriedDigest(big)
	if len(got.Summary) > memoryCarryBudgetBytes {
		t.Fatalf("carried summary = %d bytes, want <= %d", len(got.Summary), memoryCarryBudgetBytes)
	}
	if !strings.Contains(got.Summary, "NEWEST-GENERATION-TAIL") {
		t.Fatal("bounded carry lost the newest generation")
	}
	if strings.Contains(got.Summary, "OLDEST-GENERATION-HEAD") {
		t.Fatal("bounded carry kept the oldest head instead of the newest tail")
	}
	if !strings.Contains(got.Summary, "[... earlier summary omitted ...]") {
		t.Fatal("bounded carry missing the omission marker")
	}
	if len(got.KeyFacts) != 1 || len(got.OpenTasks) != 1 {
		t.Fatalf("bullets changed: facts=%v tasks=%v", got.KeyFacts, got.OpenTasks)
	}
}

func TestBoundCarriedDigestCapsFragmentProjection(t *testing.T) {
	var fragments []domain.MemoryFragment
	for i := 0; i < 8; i++ {
		fragments = append(fragments, domain.MemoryFragment{
			SourceSnapshot: domain.ContentHash("sha256:" + strings.Repeat(string(rune('a'+i)), 64)),
			Summary:        strings.Repeat(string(rune('A'+i)), 80<<10),
		})
	}
	got := boundCarriedDigest(domain.MemoryDigest{Fragments: fragments})
	total := 0
	for _, fragment := range got.Fragments {
		total += len(fragment.Summary)
	}
	if total > memoryCarryBudgetBytes {
		t.Fatalf("fragment projection = %d bytes, want <= %d", total, memoryCarryBudgetBytes)
	}
	if len(got.Fragments) == 0 || !strings.Contains(got.Fragments[len(got.Fragments)-1].Summary, "H") {
		t.Fatal("fragment projection did not retain newest contribution")
	}
}

func TestBoundCarriedDigestBoundsStructuredListsFromNewestTail(t *testing.T) {
	d := domain.MemoryDigest{
		KeyFacts:           []string{"old fact remains", strings.Repeat("oversized fact ", memoryCarryListBudgetBytes), "new fact remains"},
		OpenTasks:          []string{"old task remains", strings.Repeat("oversized task ", memoryCarryListBudgetBytes), "new task remains"},
		TasksAuthoritative: true,
	}
	got := boundCarriedDigest(d)
	if len(got.KeyFacts) != 1 || got.KeyFacts[0] != "new fact remains" {
		t.Fatalf("facts carry = %v, want newest tail", got.KeyFacts)
	}
	if len(got.OpenTasks) != 1 || got.OpenTasks[0] != "new task remains" {
		t.Fatalf("tasks carry = %v, want newest tail", got.OpenTasks)
	}
}
