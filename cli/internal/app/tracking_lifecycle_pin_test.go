package app

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"strings"
	"testing"
)

func TestTrackingIgnoresRenameMemory(t *testing.T) {
	f := newPRPositionMemoryFixture(t)
	ctx := context.Background()
	old := f.memory(f.receipt.Source, "", "recorded code pin")
	later := f.memory(f.receipt.Source, old, "later rename memory")
	birth := f.observation(old)
	birth.Kind = "birth"
	rename := birth
	rename.ID, rename.Kind, rename.PreviousBranch, rename.Branch, rename.BindingParent = strings.Repeat("7", 32), "rename", birth.Branch, "renamed", birth.ID
	rename.MemoryHash = later
	remote := []domain.HistoryEvent{birth, rename}
	if _, err := domain.ProjectContextBranches(remote); err != nil {
		t.Fatalf("invalid lifecycle fixture: %v", err)
	}
	snap, err := f.store.GetSnapshot(ctx, birth.Target)
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.Ref{RepoID: birth.RepoID, Kind: domain.RefBranch, Name: rename.Branch, BranchID: birth.BranchID, Target: birth.Target}
	event := birth
	event.ID, event.Branch = strings.Repeat("e", 32), "local-task"
	got, err := f.svc.PrepareTrackingAttachment(ctx, event, inbound.RemoteBranchObservation{Ref: ref, History: remote, Snapshots: []domain.Snapshot{snap}}, []string{event.GitAfter})
	if err != nil {
		t.Fatal(err)
	}
	if got.Event.MemoryHash != old {
		t.Fatalf("tracking accepted lifecycle-only pin: selected_later_M2=%v want_recorded_M1=%s got=%s", got.Event.MemoryHash == later, old, got.Event.MemoryHash)
	}
}
