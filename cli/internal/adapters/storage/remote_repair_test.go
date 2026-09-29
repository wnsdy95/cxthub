package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func remoteRepairFixture(t *testing.T) (*FileStore, outbound.RemoteRepairPlan) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	repo := string(domain.HashContent([]byte(t.Name())))
	st := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40))
	var ids []domain.ContentHash
	for _, session := range []string{"before", "after", "later"} {
		id, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: session}}})
		if err != nil {
			t.Fatal(err)
		}
		if err = st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	before := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: ids[0]}
	after := before
	after.Target = ids[1]
	if err := st.PutRef(ctx, before); err != nil {
		t.Fatal(err)
	}
	if err := st.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: st.gitCommit, Snapshot: ids[0]}); err != nil {
		t.Fatal(err)
	}
	observation := outbound.RemoteObservation{Version: 1, RepoID: repo, Remote: "https://verified.test/api/v1", Refs: []domain.Ref{after}}
	if err := st.CompareAndSwapRemoteObservation(ctx, "", observation); err != nil {
		t.Fatal(err)
	}
	observed, err := st.ReadRemoteObservation(ctx, repo, observation.Remote)
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.ReadCheckoutState(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	plan := outbound.RemoteRepairPlan{Version: 1, RepoID: repo, Remote: observation.Remote, Reason: "reviewed", Observation: observed.Revision, Expected: state, BeforeRef: &before, AfterRef: &after}
	plan.ID = outbound.RemoteRepairPlanID(plan)
	return st, plan
}
func TestRemoteRepairPreservesBeforeAndReplaysLostReceipt(t *testing.T) {
	st, plan := remoteRepairFixture(t)
	ctx := context.Background()
	if _, err := st.ApplyRemoteRepair(ctx, plan); err != nil {
		t.Fatal(err)
	}
	old, err := st.GetRef(ctx, plan.RepoID, domain.RefTag, "cxt-retained/repair/"+hexOf(plan.ID))
	if err != nil || old.Target != plan.BeforeRef.Target {
		t.Fatalf("lost old tip: %+v %v", old, err)
	}
	state, err := st.ReadCheckoutState(ctx, plan.RepoID)
	if err != nil || !reflect.DeepEqual(state, plan.Expected) {
		t.Fatalf("repair moved worktree: %+v %v", state, err)
	}
	// Simulate pointer committed but response/receipt lost. The exact original
	// plan can finish once; it cannot blindly overwrite another writer.
	if err := os.Remove(filepath.Join(st.storeDir(), "repair-receipts", hexOf(plan.ID)+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyRemoteRepair(ctx, plan); err != nil {
		t.Fatal(err)
	}
	later := *plan.AfterRef
	later.Target = plan.BeforeRef.Target
	if err := st.PutRef(ctx, later); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyRemoteRepair(ctx, plan); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetRef(ctx, plan.RepoID, later.Kind, later.Name)
	if got.Target != later.Target {
		t.Fatal("receipt retry rewound later writer")
	}
}
func TestRemoteRepairRejectsUnobservedTargetAndConcurrentRef(t *testing.T) {
	st, plan := remoteRepairFixture(t)
	ctx := context.Background()
	bad := plan
	after := *plan.AfterRef
	after.Target = plan.BeforeRef.Target
	bad.AfterRef = &after
	bad.ID = outbound.RemoteRepairPlanID(bad)
	if _, err := st.ApplyRemoteRepair(ctx, bad); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("unobserved target: %v", err)
	}
	id, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: "concurrent"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: plan.RepoID}); err != nil {
		t.Fatal(err)
	}
	concurrent := *plan.BeforeRef
	concurrent.Target = id
	if err = st.PutRef(ctx, concurrent); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ApplyRemoteRepair(ctx, plan); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("concurrent ref overwritten: %v", err)
	}
}
