package domain

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMemoryPositionsExactRecordedSelection(t *testing.T) {
	snap, other := HashContent([]byte("selected")), HashContent([]byte("other"))
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	publish := HistoryEvent{ID: "publication", Kind: "publish", Source: snap, Target: snap, GitAfter: a, Branch: "topic"}
	position := HistoryEvent{ID: "position", Kind: "position", Target: snap, GitAfter: b, Branch: "main", CreatedAt: time.Now()}
	receipt := HistoryEvent{ID: "receipt", Kind: "pr-merge", Source: snap, Target: snap, PRCompleted: true, Branch: "main", PR: &PullRequestMerge{Number: 12, HeadSHA: a, MergeSHA: b}}
	unrelated := position
	unrelated.Target = other
	pending := receipt
	pending.PRCompleted = false
	invalid := publish
	invalid.GitAfter = strings.Repeat("0", 40)
	birth := publish
	birth.Kind = "birth"
	birth.ID = "birth"
	advance := publish
	advance.Kind = "advance"
	advance.GitAfter = b
	tests := []struct {
		name, event  string
		history      []HistoryEvent
		reason, code string
		count        int
	}{
		{"one publication", "", []HistoryEvent{publish}, "unique", a, 1},
		{"no shared head inference", "", []HistoryEvent{unrelated}, "unavailable", "", 0},
		{"parallel positions are ambiguous", "", []HistoryEvent{publish, position}, "ambiguous", "", 2},
		{"explicit old publication", "publication", []HistoryEvent{publish, position}, "selected_event", a, 2},
		{"merge uses merge sha", "receipt", []HistoryEvent{publish, receipt}, "selected_event", b, 2},
		{"failed promotion is not evidence", "receipt", []HistoryEvent{publish, pending}, "unavailable", "", 1},
		{"selected missing event never falls back", "absent", []HistoryEvent{publish}, "unavailable", "", 1},
		{"unrelated event never falls back", "position", []HistoryEvent{publish, unrelated}, "unavailable", "", 1},
		{"branch creation", "birth", []HistoryEvent{birth}, "selected_event", a, 1},
		{"same sha is unambiguous", "", []HistoryEvent{publish, birth}, "unique", a, 2},
		{"invalid code excluded", "", []HistoryEvent{invalid}, "unavailable", "", 0},
		{"continuation is not publication", "", []HistoryEvent{advance}, "unavailable", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveMemoryPositions(snap, tt.event, tt.history)
			if got.Reason != tt.reason || got.CodeCommit != tt.code || len(got.Options) != tt.count {
				t.Fatalf("unexpected result: %+v", got)
			}
			reversed := append([]HistoryEvent{}, tt.history...)
			for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
				reversed[i], reversed[j] = reversed[j], reversed[i]
			}
			if !reflect.DeepEqual(got, ResolveMemoryPositions(snap, tt.event, reversed)) {
				t.Fatal("input order changed the selection")
			}
		})
	}
}
