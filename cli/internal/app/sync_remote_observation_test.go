package app

import (
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestRemoteBranchObservationOwnsHistoryAndGraphFromItsFetch(t *testing.T) {
	f := newResolvingBranchFixture(t)
	defer f.unchanged(t)()
	birth, _, archive, _ := f.bindingEvents()
	birth.Creation = &domain.GitCreation{Evidence: "unavailable"}
	f.remote.history = []domain.HistoryEvent{birth}
	replacement := []domain.HistoryEvent{birth, archive}
	got, err := f.svc.ResolveRemoteBranchObservation(f.ctx, inbound.SyncInput{RepoID: f.repo, Progress: func(p inbound.SyncProgress) {
		if p.Phase != "complete" {
			return
		}
		old, e := f.store.ReadRemoteObservation(f.ctx, f.repo, "configured")
		if e != nil {
			t.Fatal(e)
		}
		next := old
		next.History = replacement
		if e := f.store.CompareAndSwapRemoteObservation(f.ctx, old.Revision, next); e != nil {
			t.Fatal(e)
		}
	}}, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != f.ref || !reflect.DeepEqual(got.History, []domain.HistoryEvent{birth}) {
		t.Fatalf("mixed observations: %+v", got)
	}
	if len(got.Snapshots) != 1 || got.Snapshots[0].ID != f.snap.ID {
		t.Fatalf("missing own graph: %+v", got.Snapshots)
	}
	got.History[0].Creation.Evidence = "changed"
	if f.remote.history[0].Creation.Evidence != "unavailable" {
		t.Fatal("caller mutated transport evidence")
	}
	f.calls(t, 1, 1)
}

func TestRemoteBranchObservationUsesWarmObservedGraphNotLocalGrafts(t *testing.T) {
	f := newResolvingBranchFixture(t)
	doc := pullDoc(t, "remote child")
	child := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: f.repo, Branch: "main", Parents: []domain.ContentHash{f.snap.ID}, Models: []string{"synthetic-model"}}
	f.remote.docs = append(f.remote.docs, doc)
	f.remote.snapshots = append(f.remote.snapshots, child)
	f.remote.refs[0].Target = child.ID
	birth, _, _, _ := f.bindingEvents()
	f.remote.history = []domain.HistoryEvent{birth}
	first, err := f.svc.ResolveRemoteBranchObservation(f.ctx, inbound.SyncInput{RepoID: f.repo}, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Snapshots) != 2 {
		t.Fatalf("cold graph: %+v", first.Snapshots)
	}
	// A valid warm response may omit unchanged snapshot metadata. Its baseline
	// is the retained server observation, never a later mutable local graft.
	f.remote.snapshots = nil
	f.remote.docs = nil
	got, err := f.svc.ResolveRemoteBranchObservation(f.ctx, inbound.SyncInput{RepoID: f.repo}, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Snapshots, first.Snapshots) {
		t.Fatal("warm response lost verified catalog")
	}
	for i := range got.Snapshots {
		if got.Snapshots[i].ID == child.ID {
			got.Snapshots[i].Models[0] = "changed"
			got.Snapshots[i].Parents[0] = domain.HashContent([]byte("changed"))
		}
	}
	if child.Models[0] != "synthetic-model" || child.Parents[0] != f.snap.ID {
		t.Fatal("caller mutated graph provenance")
	}
}
