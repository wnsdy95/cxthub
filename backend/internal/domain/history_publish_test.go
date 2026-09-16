package domain

import (
	"strings"
	"testing"
	"time"
)

func TestPublishHistoryRequiresFinalizedSourceAndFullRevision(t *testing.T) {
	source := HashContent([]byte("finalized source"))
	valid := HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(HashContent([]byte("repo"))), BranchID: "feature", Branch: "feature/x", Kind: "publish", Source: source, Target: source, GitAfter: strings.Repeat("a", 40), MemoryPinned: true, CreatedAt: time.Now().UTC()}
	for _, size := range []int{40, 64} {
		e := valid
		e.GitAfter = strings.Repeat("a", size)
		if err := ValidateHistoryEvent(e); err != nil {
			t.Fatalf("valid %d-character publication: %v", size, err)
		}
		if IsBranchBindingEvent(e) {
			t.Fatal("publication changes branch lifecycle")
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*HistoryEvent)
	}{
		{"empty source", func(e *HistoryEvent) { e.Source = "" }},
		{"empty target", func(e *HistoryEvent) { e.Target = "" }},
		{"different targets", func(e *HistoryEvent) { e.Target = HashContent([]byte("other")) }},
		{"missing revision", func(e *HistoryEvent) { e.GitAfter = "" }},
		{"short revision", func(e *HistoryEvent) { e.GitAfter = "aaaaaaa" }},
		{"zero revision", func(e *HistoryEvent) { e.GitAfter = strings.Repeat("0", 40) }},
		{"uppercase revision", func(e *HistoryEvent) { e.GitAfter = strings.Repeat("A", 40) }},
		{"missing branch", func(e *HistoryEvent) { e.Branch = "" }},
		{"PR completion", func(e *HistoryEvent) { e.PRCompleted = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := valid
			tc.edit(&e)
			if err := ValidateHistoryEvent(e); err == nil {
				t.Fatal("invalid publication accepted")
			}
		})
	}
}
