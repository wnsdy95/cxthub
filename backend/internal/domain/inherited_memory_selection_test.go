package domain

import (
	"reflect"
	"strings"
	"testing"
)

func TestInheritedMemorySelectionPredicate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*HistoryEvent, *HistoryEvent)
		want   bool
	}{
		{"explicit inherited", func(b, a *HistoryEvent) {}, true},
		{"prior context source", func(b, a *HistoryEvent) { b.Source = b.MemorySource }, true},
		{"explicit self successor", func(b, a *HistoryEvent) { a.MemorySource = a.Target }, true},
		{"implicit predecessor owner", func(b, a *HistoryEvent) { b.MemorySource = "" }, false},
		{"self owned predecessor", func(b, a *HistoryEvent) { b.MemorySource = b.Target }, false},
		{"publish predecessor", func(b, a *HistoryEvent) { b.Kind = "publish" }, false},
		{"birth predecessor", func(b, a *HistoryEvent) { b.Kind = "birth" }, false},
		{"attach predecessor", func(b, a *HistoryEvent) { b.Kind = "attach" }, false},
		{"different worktree", func(b, a *HistoryEvent) { a.WorktreeID = strings.Repeat("9", 32) }, false},
		{"different code", func(b, a *HistoryEvent) { a.GitAfter = strings.Repeat("9", 40) }, false},
		{"different identity", func(b, a *HistoryEvent) { a.BranchID = "other" }, false},
		{"different alias", func(b, a *HistoryEvent) { a.LocalBranch = "other" }, false},
		{"different name", func(b, a *HistoryEvent) { a.Branch = "other" }, false},
		{"different repo", func(b, a *HistoryEvent) { a.RepoID = string(HashContent([]byte("other-repo"))) }, false},
		{"different target", func(b, a *HistoryEvent) { a.Target = b.MemorySource; a.Source = a.Target }, false},
		{"binding only", func(b, a *HistoryEvent) { a.BindingParent = a.MemorySelectionParent; a.MemorySelectionParent = "" }, false},
		{"wrong exact parent", func(b, a *HistoryEvent) { a.MemorySelectionParent = strings.Repeat("8", 32) }, false},
		{"unpinned predecessor", func(b, a *HistoryEvent) { b.MemoryPinned = false }, false},
		{"foreign successor", func(b, a *HistoryEvent) { a.MemorySource = b.MemorySource }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, a := memorySelectionPair()
			b.MemoryHash = HashContent([]byte("inherited"))
			b.MemorySource = HashContent([]byte("owner"))
			tc.change(&b, &a)
			if got := IsInitialMemorySelection(b, a); got != tc.want {
				t.Fatalf("predicate=%v want=%v", got, tc.want)
			}
			if tc.want {
				for _, e := range []HistoryEvent{b, a} {
					if err := ValidateHistoryEvent(e); err != nil {
						t.Fatal(err)
					}
				}
				raw := []HistoryEvent{a, b}
				ordered, err := OrderHistoryEvents(raw)
				if err != nil || len(ordered) != 2 || !reflect.DeepEqual(ordered[0], b) || !reflect.DeepEqual(ordered[1], a) {
					t.Fatalf("raw dependency/order lost: %v", err)
				}
			}
		})
	}
}
