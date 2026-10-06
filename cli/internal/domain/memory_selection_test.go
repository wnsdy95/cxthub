package domain

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func memorySelectionPair() (HistoryEvent, HistoryEvent) {
	before := HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(HashContent([]byte("repo"))), BranchID: "identity", Branch: "main", LocalBranch: "main", Kind: "position", Source: HashContent([]byte("b")), Target: HashContent([]byte("b")), MemoryPinned: true, GitAfter: strings.Repeat("c", 40), WorktreeID: strings.Repeat("d", 32), CreatedAt: time.Unix(20, 0).UTC()}
	after := before
	after.ID, after.MemorySelectionParent = strings.Repeat("2", 32), before.ID
	after.MemoryHash, after.CreatedAt = HashContent([]byte("e")), time.Unix(10, 0).UTC()
	return before, after
}
func TestMemorySelectionParentShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*HistoryEvent)
	}{
		{"bad-id", func(e *HistoryEvent) { e.MemorySelectionParent = "invalid" }},
		{"self", func(e *HistoryEvent) { e.MemorySelectionParent = e.ID }},
		{"uppercase-id", func(e *HistoryEvent) { e.MemorySelectionParent = strings.Repeat("A", 32) }},
		{"publish", func(e *HistoryEvent) { e.Kind = "publish" }},
		{"advance", func(e *HistoryEvent) { e.Kind = "advance" }},
		{"birth", func(e *HistoryEvent) { e.Kind = "birth" }},
		{"unpinned", func(e *HistoryEvent) { e.MemoryPinned = false }},
		{"empty-memory", func(e *HistoryEvent) { e.MemoryHash = "" }},
		{"missing-target", func(e *HistoryEvent) { e.Target = "" }},
		{"different-source", func(e *HistoryEvent) { e.Source = HashContent([]byte("f")) }},
		{"foreign-owner", func(e *HistoryEvent) { e.MemorySource = HashContent([]byte("f")) }},
		{"missing-worktree", func(e *HistoryEvent) { e.WorktreeID = "" }},
		{"missing-code", func(e *HistoryEvent) { e.GitAfter = "" }},
		{"zero-code", func(e *HistoryEvent) { e.GitAfter = strings.Repeat("0", 40) }},
		{"uppercase-code", func(e *HistoryEvent) { e.GitAfter = strings.Repeat("C", 40) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e := memorySelectionPair()
			tc.change(&e)
			if ValidateHistoryEvent(e) == nil {
				t.Fatal("invalid memory selection accepted")
			}
		})
	}
	before, after := memorySelectionPair()
	for _, e := range []HistoryEvent{before, after} {
		if err := ValidateHistoryEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	after.MemorySource = after.Target
	if err := ValidateHistoryEvent(after); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "memory_selection_parent") {
		t.Fatal("legacy payload gained a field")
	}
}
func TestMemorySelectionParentPredicate(t *testing.T) {
	before, after := memorySelectionPair()
	if !IsInitialMemorySelection(before, after) {
		t.Fatal("exact successor rejected")
	}
	after.MemorySource = after.Target
	if !IsInitialMemorySelection(before, after) {
		t.Fatal("explicit self owner rejected")
	}
	for _, tc := range []struct {
		name   string
		change func(*HistoryEvent, *HistoryEvent)
	}{
		{"binding-parent-only", func(b, a *HistoryEvent) { a.BindingParent = a.MemorySelectionParent; a.MemorySelectionParent = "" }},
		{"wrong-parent", func(b, a *HistoryEvent) { a.MemorySelectionParent = strings.Repeat("9", 32) }},
		{"repo", func(b, a *HistoryEvent) { a.RepoID = strings.Repeat("9", 64) }},
		{"identity", func(b, a *HistoryEvent) { a.BranchID = "other" }},
		{"branch", func(b, a *HistoryEvent) { a.Branch = "other" }},
		{"alias", func(b, a *HistoryEvent) { a.LocalBranch = "other" }},
		{"worktree", func(b, a *HistoryEvent) { a.WorktreeID = strings.Repeat("9", 32) }},
		{"code", func(b, a *HistoryEvent) { a.GitAfter = strings.Repeat("9", 40) }},
		{"target", func(b, a *HistoryEvent) { a.Source = HashContent([]byte("9")); a.Target = a.Source }},
		{"birth-predecessor", func(b, a *HistoryEvent) { b.Kind = "birth" }},
		{"nonempty-predecessor", func(b, a *HistoryEvent) { b.MemoryHash = a.MemoryHash }},
		{"unpinned-predecessor", func(b, a *HistoryEvent) { b.MemoryPinned = false }},
		{"predecessor-owner", func(b, a *HistoryEvent) { b.MemorySource = b.Target }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, a := memorySelectionPair()
			tc.change(&b, &a)
			if IsInitialMemorySelection(b, a) {
				t.Fatal("unrelated selection accepted")
			}
		})
	}
}
func TestMemorySelectionParentDependencies(t *testing.T) {
	before, after := memorySelectionPair()
	// Deduplicate a dependency used by two fields without changing their policies.
	after.BindingParent = before.ID
	if got := HistoryDependencies(after); !reflect.DeepEqual(got, []string{before.ID}) {
		t.Fatalf("duplicate dependencies: %v", got)
	}
	if got, err := OrderHistoryEvents([]HistoryEvent{after, before, after}); err != nil || len(got) != 2 || got[0].ID != before.ID {
		t.Fatalf("deduplicated order: %v %v", got, err)
	}
	before.BindingParent = after.ID
	if _, err := OrderHistoryEvents([]HistoryEvent{before, after}); err == nil {
		t.Fatal("cross-field cycle accepted")
	}
	// Ordinary BindingParent may still refer to an identity birth, and has no
	// initial-memory meaning. Do not narrow its longstanding validation here.
	before, after = memorySelectionPair()
	before.Kind = "birth"
	after.MemorySelectionParent = ""
	after.BindingParent = before.ID
	if err := ValidateHistoryEvent(after); err != nil {
		t.Fatal(err)
	}
	if _, err := OrderHistoryEvents([]HistoryEvent{after, before}); err != nil {
		t.Fatal(err)
	}
	if IsInitialMemorySelection(before, after) {
		t.Fatal("birth interpreted as memory selection")
	}
}
