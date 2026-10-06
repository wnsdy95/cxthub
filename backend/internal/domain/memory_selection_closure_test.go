package domain

import (
	"context"
	"reflect"
	"testing"
)

func TestMemorySelectionParentBranchPullClosure(t *testing.T) {
	f := bpFixture()
	before, after := memorySelectionPair()
	for _, e := range []*HistoryEvent{&before, &after} {
		e.RepoID = string(f.repo.ID)
		e.BranchID = "selected"
		e.Branch = "feature"
		e.LocalBranch = "feature"
		e.Source = f.refs[0].Target
		e.Target = f.refs[0].Target
	}
	f.history = append(f.history, after, before)
	got, err := f.plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var pair []HistoryEvent
	for _, e := range got.History {
		if e.ID == before.ID || e.ID == after.ID {
			pair = append(pair, e)
		}
	}
	if !reflect.DeepEqual(pair, []HistoryEvent{before, after}) {
		t.Fatal("branch inventory lost immutable causal evidence")
	}
	f.history = f.history[:2]
	if _, err = f.plan(context.Background()); err == nil {
		t.Fatal("branch inventory accepted missing memory predecessor")
	}
}
