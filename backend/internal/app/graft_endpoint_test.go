package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// TestGraftSnapshotParents fixes the contract for client graft propagation paths:
// Only adds existing parents (preventing reachability poisoning with arbitrary hashes), ensures idempotency, and rejects self-grafts and empty lists.
// Background: The only path for local to reflect a reachability overlay in a server replica during sibling advancement in multi-session commits (inventory-only push does not resend existing object metadata).
func TestGraftSnapshotParents(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	svc := NewService(st, st, nil, nil, st)
	repo := domain.HashContent([]byte("graft-repo"))

	mk := func(seed string) domain.ContentHash {
		id := domain.HashContent([]byte(seed))
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main", Provider: "claude"}
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		return id
	}
	head := mk("head")
	sibling := mk("sibling")

	if err := svc.GraftSnapshotParents(ctx, repo, head, []domain.ContentHash{sibling}, 0); err != nil {
		t.Fatalf("graft failed: %v", err)
	}
	got, err := st.GetSnapshot(ctx, repo, head)
	if err != nil || len(got.GraftParents) != 1 || got.GraftParents[0] != sibling {
		t.Fatalf("graft not reflected: %v %v", got.GraftParents, err)
	}
	// Idempotent retries (push queue refresh) do not add duplicates.
	if err := svc.GraftSnapshotParents(ctx, repo, head, []domain.ContentHash{sibling}, 0); err != nil {
		t.Fatalf("idempotent retry failed: %v", err)
	}
	if got, _ := st.GetSnapshot(ctx, repo, head); len(got.GraftParents) != 1 {
		t.Fatalf("duplicate addition: %v", got.GraftParents)
	}
	// Rejects non-existent parents (preventing reachability poisoning).
	ghost := domain.HashContent([]byte("ghost"))
	if err := svc.GraftSnapshotParents(ctx, repo, head, []domain.ContentHash{ghost}, 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("phantom parent allowed: %v", err)
	}
	// Reject self graft and empty list.
	if err := svc.GraftSnapshotParents(ctx, repo, head, []domain.ContentHash{head}, 1); err == nil {
		t.Fatal("Self graft allowed")
	}
	if err := svc.GraftSnapshotParents(ctx, repo, head, nil, 1); err == nil {
		t.Fatal("Empty list allowed")
	}

	// After seq is advanced by other mutations like join/supersede, the queue
	// cannot be restored to the edge. However, retries of already applied edges
	// are idempotent even if the old expected value is different.
	late := mk("late")
	if err := svc.GraftSnapshotParents(ctx, repo, head, []domain.ContentHash{late}, 0); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("Stale expected_seq allowed: %v", err)
	}
	if err := svc.GraftSnapshotParents(ctx, repo, head, []domain.ContentHash{sibling}, 0); err != nil {
		t.Fatalf("Idempotent retry of already applied edge failed: %v", err)
	}
	// If the same edge has advanced more than two steps in the register compared
	// to the event, it is a stale replay after join/other mutation. Allowing success
	// due to the edge accidentally remaining can lead to clients agreeing on different
	// projections for the same seq.
	other := mk("other")
	if err := svc.GraftSnapshotParents(ctx, repo, head, []domain.ContentHash{other}, 1); err != nil {
		t.Fatalf("Second graft failed: %v", err)
	}
	if err := svc.GraftSnapshotParents(ctx, repo, head, []domain.ContentHash{sibling}, 0); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("old idempotent retry allowed: %v", err)
	}

	a := mk("cycle-a")
	b := mk("cycle-b")
	if err := svc.GraftSnapshotParents(ctx, repo, a, []domain.ContentHash{b}, 0); err != nil {
		t.Fatalf("cycle preparation edge failure: %v", err)
	}
	if err := svc.GraftSnapshotParents(ctx, repo, b, []domain.ContentHash{a}, 0); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("reachability cycle allowed: %v", err)
	}
	gotB, _ := st.GetSnapshot(ctx, repo, b)
	if len(gotB.GraftParents) != 0 || gotB.GraftSeq != 0 {
		t.Fatalf("rejected cycle mutation stored: %+v", gotB)
	}
}
