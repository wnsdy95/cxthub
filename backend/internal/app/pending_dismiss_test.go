package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// TestPutPendingRejectsMissingTarget fixes pointer/object push atomicity defense —
// A pending pointer pointing to a non-existent snapshot is rejected (fail-closed). Without this, the list shows
// an orphan session with no node in the graph.
func TestPutPendingRejectsMissingTarget(t *testing.T) {
	svc, _ := newFsckSvc(t)
	err := svc.PutPending(context.Background(), hh("r"), "sess-y", domain.Pending{Target: hh("z"), Branch: "main"})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err=%v, want ErrValidation (missing target requires object push)", err)
	}
}

// TestDismissPendingSticky fixes dismiss behavior —
// (1) It only sets the dismissed flag without deleting data,
// (2) It does not revive via CLI re-push (SyncPendings — overwrite of pointers without dismissed flag).
// This is the core defense against the "deleted but still appearing" issue.
func TestDismissPendingSticky(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("r")
	sid := "sess-x"
	// PutPending validates target object existence (fail-closed) — mirror hook capture snapshot push.
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: hh("t"), RepoID: repo, DocHash: hh("t"), Message: "hook: wip"}); err != nil {
		t.Fatal(err)
	}
	push := func() { // CLI hook/re-push mirror
		if err := svc.PutPending(ctx, repo, sid, domain.Pending{Target: hh("t"), Branch: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	push()

	if err := svc.DismissPending(ctx, repo, sid); err != nil {
		t.Fatal(err)
	}
	push() // re-push: must be sticky even with pointers without dismissed flag

	pendings, err := svc.ListPendings(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(pendings) != 1 {
		t.Fatalf("pending count=%d, want 1 (not deleted)", len(pendings))
	}
	if !pendings[0].Dismissed {
		t.Fatal("dismissed reset by re-push (sticky failure) — reappears in list")
	}
	if pendings[0].Target != hh("t") {
		t.Fatalf("data(target) loss: %v — dismiss must be non-destructive", pendings[0].Target)
	}
}

// TestUndismissPending fixes the dismiss operation: un-dismiss + list recovery + CLI re-push
// sticky is cleared (pendingDismissed=false path).
func TestUndismissPending(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("r")
	sid := "sess-u"
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: hh("u"), RepoID: repo, DocHash: hh("u"), Message: "hook: wip"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.PutPending(ctx, repo, sid, domain.Pending{Target: hh("u"), Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DismissPending(ctx, repo, sid); err != nil {
		t.Fatal(err)
	}
	if err := svc.UndismissPending(ctx, repo, sid); err != nil {
		t.Fatal(err)
	}
	ps, _ := svc.ListPendings(ctx, repo)
	if len(ps) != 1 || ps[0].Dismissed {
		t.Fatalf("still hidden after undismiss: %+v", ps)
	}
	// Ensure re-push does not get re-contaminated by dismiss (sticky un-set check).
	if err := svc.PutPending(ctx, repo, sid, domain.Pending{Target: hh("u"), Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	ps, _ = svc.ListPendings(ctx, repo)
	if ps[0].Dismissed {
		t.Fatal("sticky revived after re-push after undismiss")
	}
}
