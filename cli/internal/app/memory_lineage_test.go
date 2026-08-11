package app

import (
	"context"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// TestMergeDigests fixes the determinism rules for memory digest merges:
// ancestor precedence + dedup, disallow duplicate links if summaries are contained.
func TestMergeDigests(t *testing.T) {
	prior := domain.MemoryDigest{Summary: "Previous session summary", KeyFacts: []string{"A", "B"}, OpenTasks: []string{"t1"}}
	fresh := domain.MemoryDigest{Summary: "New session summary", KeyFacts: []string{"B", "C"}, OpenTasks: []string{"t1", "t2"}}
	m := domain.MergeDigests(prior, fresh)
	if !strings.Contains(m.Summary, "Previous session summary") || !strings.Contains(m.Summary, "New session summary") {
		t.Fatalf("Summary merge failure: %q", m.Summary)
	}
	if got := strings.Join(m.KeyFacts, ","); got != "A,B,C" {
		t.Fatalf("key_facts merge: %s", got)
	}
	if got := strings.Join(m.OpenTasks, ","); got != "t1,t2" {
		t.Fatalf("open_tasks merge: %s", got)
	}
	// Avoid double linking summaries if fresh contains prior summary.
	same := domain.MergeDigests(prior, domain.MemoryDigest{Summary: "previous session summary + addition"})
	if strings.Count(same.Summary, "previous session summary") != 1 {
		t.Fatalf("inclusion summary has duplicate links: %q", same.Summary)
	}
}

// TestNearestAncestorDigest fixes reachability parent chain BFS traversal: skips memory-less intermediate
// snapshots and finds the nearest ancestor digest.
func TestNearestAncestorDigest(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	h := func(c byte) domain.ContentHash {
		return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
	}
	// A(with memory) ← B(without) ← C. When searching from C, the digest of A should be returned.
	memHash, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: h('a'), Summary: "ancestor memory", KeyFacts: []string{"fact-a"}})
	if err != nil {
		t.Fatal(err)
	}
	put := func(id domain.ContentHash, mem domain.ContentHash, parents ...domain.ContentHash) {
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, Branch: "main", Parents: parents, MemoryHash: mem}); err != nil {
			t.Fatal(err)
		}
	}
	put(h('a'), memHash)
	put(h('b'), "", h('a'))
	put(h('c'), "", h('b'))

	c, _ := st.GetSnapshot(ctx, h('c'))
	d, ok := nearestAncestorDigest(ctx, st, c)
	if !ok || d.Summary != "ancestor memory" {
		t.Fatalf("inheritance failed: ok=%v summary=%q", ok, d.Summary)
	}
	// Root (no parent) has no successor.
	a, _ := st.GetSnapshot(ctx, h('a'))
	if _, ok := nearestAncestorDigest(ctx, st, a); ok {
		t.Fatal("Successor occurred at root")
	}

	// Immutable overlay graft: D has no natural parent and A is only connected via GraftParents.
	// Starting from E, A's memory must be found even after passing through memoryless D.
	if err := st.PutSnapshot(ctx, domain.Snapshot{
		ID: h('d'), DocHash: h('d'), Branch: "main", Grafted: true,
		GraftParents: []domain.ContentHash{h('a')},
	}); err != nil {
		t.Fatal(err)
	}
	put(h('e'), "", h('d'))
	e, _ := st.GetSnapshot(ctx, h('e'))
	d, ok = nearestAncestorDigest(ctx, st, e)
	if !ok || d.Summary != "ancestor memory" {
		t.Fatalf("overlay graft inheritance failed: ok=%v summary=%q", ok, d.Summary)
	}
}

// TestBoundCarriedDigestKeepsNewestTail (#33 — bounded carry): the forward
// working set is capped at memoryCarryBudgetBytes keeping the newest tail;
// under-budget digests pass through unchanged.
func TestBoundCarriedDigestKeepsNewestTail(t *testing.T) {
	small := domain.MemoryDigest{Summary: "compact history", KeyFacts: []string{"f"}}
	if got := boundCarriedDigest(small); got.Summary != small.Summary {
		t.Fatalf("under-budget summary changed: %q", got.Summary)
	}

	big := domain.MemoryDigest{
		Summary:   "OLDEST-GENERATION-HEAD\n" + strings.Repeat("m", 2*memoryCarryBudgetBytes) + "\nNEWEST-GENERATION-TAIL",
		KeyFacts:  []string{"fact stays"},
		OpenTasks: []string{"task stays"},
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

// A fresh generation is not a carried ancestor and must remain byte-for-byte
// intact even when it exceeds the carry budget. Only the inherited side is
// bounded before MergeDigests.
func TestBoundedCarryDoesNotTruncateFreshDigest(t *testing.T) {
	prior := domain.MemoryDigest{Summary: strings.Repeat("old", memoryCarryBudgetBytes)}
	freshText := "FRESH-HEAD\n" + strings.Repeat("new", memoryCarryBudgetBytes) + "\nFRESH-TAIL"
	fresh := domain.MemoryDigest{Summary: freshText}
	merged := domain.MergeDigests(boundCarriedDigest(prior), fresh)
	if !strings.Contains(merged.Summary, freshText) {
		t.Fatal("fresh digest was truncated while limiting inherited carry")
	}
}

func TestBoundCarriedDigestBoundsStructuredListsFromNewestTail(t *testing.T) {
	d := domain.MemoryDigest{
		KeyFacts:  []string{"old-fact", strings.Repeat("f", memoryCarryListBudgetBytes), "new-fact"},
		OpenTasks: []string{"old-task", strings.Repeat("t", memoryCarryListBudgetBytes), "new-task"},
	}
	got := boundCarriedDigest(d)
	if len(got.KeyFacts) != 1 || got.KeyFacts[0] != "new-fact" {
		t.Fatalf("facts carry = %v, want newest tail", got.KeyFacts)
	}
	if len(got.OpenTasks) != 1 || got.OpenTasks[0] != "new-task" {
		t.Fatalf("tasks carry = %v, want newest tail", got.OpenTasks)
	}
}
