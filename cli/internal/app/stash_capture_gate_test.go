package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type stashGateRestore struct {
	load func(context.Context, inbound.LoadInput) (inbound.LoadOutput, error)
}

func (r stashGateRestore) Load(ctx context.Context, in inbound.LoadInput) (inbound.LoadOutput, error) {
	return r.load(ctx, in)
}

// The capture is already durable when Stash enters Load. Production Load uses
// the configured agent preparer and fresh server history; it must not retain
// first-setup capture exclusion during that remote/restoration phase.
func TestStashReleasesCaptureGateBeforeRestore(t *testing.T) {
	t.Run("success", func(t *testing.T) { testStashRestoreGate(t, nil) })
	t.Run("restore-failure", func(t *testing.T) { testStashRestoreGate(t, context.Canceled) })
}

func testStashRestoreGate(t *testing.T, restoreErr error) {
	f := newStagingFixture(t)
	ctx := context.Background()
	head := pullDoc(t, "existing head distinct from stash")
	if _, err := f.store.PutDoc(ctx, head); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutSnapshot(ctx, domain.Snapshot{ID: head.Hash, DocHash: head.Hash, RepoID: f.git.repo.ID, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateBranchRef(ctx, domain.Ref{RepoID: f.git.repo.ID, Kind: domain.RefBranch, Name: "main", Target: head.Hash}); err != nil {
		t.Fatal(err)
	}
	source := f.source(t, "test-owned-stash", "uncommitted local work")
	called := false
	load := stashGateRestore{load: func(_ context.Context, _ inbound.LoadInput) (inbound.LoadOutput, error) {
		called = true
		stack, err := f.store.StashList(ctx, f.git.repo.ID)
		if err != nil || len(stack) != 1 {
			t.Fatalf("restore began before durable stash: %+v %v", stack, err)
		}
		limited, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		pristine, err := f.store.TrackingPristine(limited, f.git.repo.ID)
		if err != nil || pristine {
			t.Errorf("setup admission blocked during post-capture restore: pristine=%v err=%v; want immediate non-pristine result", pristine, err)
		}
		return inbound.LoadOutput{ResumeCmd: "resume synthetic-stash"}, restoreErr
	}}
	svc := NewStashService(f.git, f.svc.save.captures, f.svc.save.codecs, f.store, load, f.svc.save.capture)
	out, err := svc.Stash(ctx, inbound.StashInput{Cwd: f.root, Provider: source.Provider, SessionPath: source.Path})
	if err != nil {
		t.Fatal(err)
	}
	stack, err := f.store.StashList(ctx, f.git.repo.ID)
	if err != nil || len(stack) != 1 || stack[0].Snapshot != out.StashID || out.Depth != 1 {
		t.Fatalf("stash lost after restore: %+v %+v %v", out, stack, err)
	}
	if out.RestoredHead != (restoreErr == nil) {
		t.Fatalf("restore result: %+v", out)
	}
	if restoreErr == nil && out.ResumeCmd != "resume synthetic-stash" {
		t.Fatalf("resume output lost: %+v", out)
	}
	if restoreErr != nil && out.ResumeCmd != "" {
		t.Fatalf("failed restore leaked success output: %+v", out)
	}
	if !called {
		t.Fatal("fixture did not reach restore")
	}
	pristine, err := f.store.TrackingPristine(ctx, f.git.repo.ID)
	if err != nil || pristine {
		t.Fatalf("admission after restore: %v %v", pristine, err)
	}
}

// Observe both sides of the real stack publication, before optional restore.
// An early gate release would let either admission attempt acquire exclusion.
type stashPublicationGateStore struct {
	*storage.FileStore
	t       *testing.T
	checked bool
}

func (s *stashPublicationGateStore) StashPush(ctx context.Context, repo string, entry domain.StashEntry) error {
	assertHeld := func() {
		s.t.Helper()
		limited, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		if _, err := s.TrackingPristine(limited, repo); !errors.Is(err, context.DeadlineExceeded) {
			s.t.Errorf("capture gate released before stash publication completed: %v", err)
		}
	}
	assertHeld()
	if err := s.FileStore.StashPush(ctx, repo, entry); err != nil {
		return err
	}
	assertHeld()
	s.checked = true
	return nil
}

func TestStashCaptureGateCoversPublication(t *testing.T) {
	f := newStagingFixture(t)
	store := &stashPublicationGateStore{FileStore: f.store, t: t}
	source := f.source(t, "publication-owned-session", "local stash to retain")
	svc := NewStashService(f.git, f.svc.save.captures, f.svc.save.codecs, store, nil, f.svc.save.capture)
	if _, err := svc.Stash(context.Background(), inbound.StashInput{Cwd: f.root, Provider: source.Provider, SessionPath: source.Path}); err != nil {
		t.Fatal(err)
	}
	if !store.checked {
		t.Fatal("stash publication not reached")
	}
}
