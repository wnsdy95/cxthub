package domain

import (
	"encoding/json"
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
