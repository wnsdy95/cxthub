package domain

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func activityPublication() HistoryEvent {
	snapshot := HashContent([]byte("old snapshot"))
	return HistoryEvent{
		ID: strings.Repeat("1", 32), RepoID: string(HashContent([]byte("repo"))),
		Branch: "main", BranchID: "main", Kind: "publish", Source: snapshot, Target: snapshot,
		GitAfter: strings.Repeat("a", 40), CreatedAt: time.Date(2026, 8, 31, 23, 30, 0, 0, time.FixedZone("west", -7*3600)),
	}
}

func TestCommitActivityDaysDeduplicatesRevisionsNotSnapshots(t *testing.T) {
	first := activityPublication()
	reused := first
	reused.ID, reused.GitAfter = strings.Repeat("2", 32), strings.Repeat("b", 40)
	reused.CreatedAt = time.Date(2026, 10, 1, 0, 30, 0, 0, time.FixedZone("east", 2*3600))
	duplicate := first
	duplicate.ID, duplicate.Branch, duplicate.BranchID = strings.Repeat("3", 32), "feature", "feature"
	duplicate.Source, duplicate.Target = HashContent([]byte("another snapshot")), HashContent([]byte("another snapshot"))
	duplicate.WorktreeID = strings.Repeat("4", 32)
	duplicate.CreatedAt = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	history := []HistoryEvent{duplicate, reused, first, first}
	want := map[string]int{"2026-09-01": 1, "2026-09-30": 1}
	for range 2 {
		got, err := CommitActivityDays(ContentHash(first.RepoID), history)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("days = %v, %v; want %v", got, err, want)
		}
		slices.Reverse(history)
	}
}

func TestCommitActivityDaysRequiresFinalization(t *testing.T) {
	for _, kind := range []string{"birth", "attach", "orphan", "position", "advance", "rename", "archive", "pr-merge"} {
		t.Run(kind, func(t *testing.T) {
			event := activityPublication()
			event.Kind = kind
			got, err := CommitActivityDays(ContentHash(event.RepoID), []HistoryEvent{event})
			if err != nil || got == nil || len(got) != 0 {
				t.Fatalf("unfinalized history counted: %v, %v", got, err)
			}
		})
	}
}

func TestCommitActivityDaysRejectsInvalidPublication(t *testing.T) {
	for name, mutate := range map[string]func(*HistoryEvent){
		"missing date":       func(event *HistoryEvent) { event.CreatedAt = time.Time{} },
		"missing revision":   func(event *HistoryEvent) { event.GitAfter = "" },
		"zero revision":      func(event *HistoryEvent) { event.GitAfter = strings.Repeat("0", 40) },
		"mismatched target":  func(event *HistoryEvent) { event.Target = HashContent([]byte("other")) },
		"foreign repository": func(event *HistoryEvent) { event.RepoID = string(HashContent([]byte("foreign"))) },
	} {
		t.Run(name, func(t *testing.T) {
			event := activityPublication()
			repo := ContentHash(event.RepoID)
			mutate(&event)
			if _, err := CommitActivityDays(repo, []HistoryEvent{event}); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("invalid publication accepted: %v", err)
			}
		})
	}
}
