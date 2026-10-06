package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func firstTrackingFixture(t *testing.T) trackingFixture {
	t.Helper()
	f := newTrackingFixture(t)
	for _, path := range []string{filepath.Join(f.store.storeDir(), "refs"), filepath.Join(f.store.storeDir(), "history"), filepath.Join(f.store.storeDir(), "worktrees"), f.store.objectPath("snapshots", f.old), f.store.objectPath("docs", f.old)} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	f.commit.RequirePristine = true
	f.commit.Position.Expected = nil
	f.commit.Position.Next.Selection.Source = ""
	f.commit.Position.Next.Selection.GitBefore = ""
	for _, id := range []domain.ContentHash{f.chosen, f.shared} {
		snap, err := f.store.GetSnapshot(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		f.commit.ObservedSnapshots = append(f.commit.ObservedSnapshots, snap)
	}
	obs := outbound.RemoteObservation{Version: 1, RepoID: f.commit.Attachment.Event.RepoID, Remote: "fixture", Snapshots: f.commit.ObservedSnapshots, Refs: []domain.Ref{f.commit.Attachment.ObservedRef}, History: f.commit.Attachment.Proof}
	if err := f.store.CompareAndSwapRemoteObservation(context.Background(), "", obs); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFirstTrackingAdmissionAndReceipt(t *testing.T) {
	f := firstTrackingFixture(t)
	ctx := context.Background()
	repo := f.commit.Attachment.Event.RepoID
	pristine, err := f.store.TrackingPristine(ctx, repo)
	if err != nil || !pristine {
		t.Fatalf("preflight=%v %v", pristine, err)
	}
	if err = f.store.CommitTrackingAttachment(ctx, f.commit); err != nil {
		t.Fatal(err)
	}
	p, err := f.store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Snapshot, p.SharedTarget, p.MemoryHash, p.MemorySource, p.Selection, p.Rewound = f.shared, f.shared, "", "", nil, false
	if err = f.store.PutWorkingPosition(ctx, p); err != nil {
		t.Fatal(err)
	}
	before, err := f.store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Exact replay bypasses new pristine checks and cannot restore the old selection.
	if err = f.store.CommitTrackingAttachment(ctx, f.commit); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.GetWorkingPosition(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("receipt rolled back newer position", err)
	}
}

func TestFirstTrackingRejectsPhysicalForeignState(t *testing.T) {
	for _, kind := range []string{"pending", "snapshot", "document", "staging", "corrupt-position", "symlink", "changed-observed-metadata"} {
		t.Run(kind, func(t *testing.T) {
			f := firstTrackingFixture(t)
			ctx := context.Background()
			foreign := string(domain.HashContent([]byte("provisional")))
			switch kind {
			case "pending":
				if err := f.store.PutPending(ctx, domain.Pending{RepoID: foreign, SessionID: "s", Target: f.chosen, Branch: "local-task"}); err != nil {
					t.Fatal(err)
				}
			case "snapshot":
				trackingSnapshot(t, f.store, foreign, "unpublished foreign snapshot")
			case "document":
				if _, err := f.store.PutDoc(ctx, domain.SessionDoc{CIR: sampleCIR("unpublished document")}); err != nil {
					t.Fatal(err)
				}
			case "staging":
				trackingJSON(t, filepath.Join(f.store.storeDir(), "worktrees", f.store.worktreeID, "index.json"), map[string]any{"repo_id": foreign})
			case "corrupt-position":
				trackingJSON(t, f.store.positionPath(), map[string]any{"worktree_id": "wrong"})
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(f.store.storeDir(), "pending")); err != nil {
					t.Fatal(err)
				}
			case "changed-observed-metadata":
				snap, err := f.store.GetSnapshot(ctx, f.chosen)
				if err != nil {
					t.Fatal(err)
				}
				snap.GraftParents = []domain.ContentHash{f.shared}
				snap.GraftSeq = 1
				if err = f.store.PutSnapshot(ctx, snap); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.store.CommitTrackingAttachment(ctx, f.commit); err == nil {
				t.Fatal("accepted non-pristine state")
			}
			if _, err := os.Stat(f.store.trackingAttachmentPath()); !os.IsNotExist(err) {
				t.Fatal("accepted journal for rejected setup")
			}
			if _, err := f.store.readLocalBinding(f.commit.Attachment.Event.RepoID, "local-task"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("binding written", err)
			}
		})
	}
}

func TestFirstTrackingCaptureGateAcrossFileStores(t *testing.T) {
	f := firstTrackingFixture(t)
	ctx := context.Background()
	peer := NewWorktreeFileStore(f.store.repoRoot, filepath.Join(f.store.repoRoot, ".git"), "local-task", f.store.gitCommit)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- peer.WithCaptureTrackingGate(ctx, func(locked context.Context) error {
			close(entered)
			<-release
			_, err := peer.ReplacePending(locked, domain.Pending{RepoID: f.commit.Attachment.Event.RepoID, SessionID: "capture", Target: f.chosen, Branch: "local-task"})
			return err
		})
	}()
	<-entered
	deadline, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	err := f.store.CommitTrackingAttachment(deadline, f.commit)
	cancel()
	close(release)
	captureErr := <-done
	if !errors.Is(err, context.DeadlineExceeded) || captureErr != nil {
		t.Fatalf("capture gate lost ordering: %v %v", err, captureErr)
	}
	if err := f.store.CommitTrackingAttachment(ctx, f.commit); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("captured pending overwritten: %v", err)
	}
}

func TestFirstTrackingSnapshotFenceAndLockRelease(t *testing.T) {
	f := firstTrackingFixture(t)
	ctx := context.Background()
	peer := NewFileStore(f.store.repoRoot)
	err := func() error {
		_, err := f.store.withOSLock(ctx, "first-tracking", "snapshots", syscall.LOCK_EX, true, func() error {
			limited, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
			defer cancel()
			snap := f.commit.ObservedSnapshots[0]
			if err := peer.PutSnapshot(limited, snap); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("snapshot bypassed catalog fence: %v", err)
			}
			return nil
		})
		return err
	}()
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.PutSnapshot(ctx, f.commit.ObservedSnapshots[0]); err != nil {
		t.Fatal("lock was not released", err)
	}
	if err := f.store.CommitTrackingAttachment(ctx, f.commit); err != nil {
		t.Fatal(err)
	}
}

func TestFirstTrackingRecoveryEveryAcceptedPrefix(t *testing.T) {
	for step := 0; step <= 7; step++ {
		t.Run(fmt.Sprint(step), func(t *testing.T) {
			f := firstTrackingFixture(t)
			j := f.journal(t)
			f.prefix(t, j, step)
			// Recovery uses accepted proof, not a new fetch or a now-false pristine scan.
			if _, err := f.store.GetWorkingPosition(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := f.store.CommitTrackingAttachment(context.Background(), f.commit); err != nil {
				t.Fatal(err)
			}
			p, err := f.store.GetWorkingPosition(context.Background())
			if err != nil || p.Snapshot != f.chosen || p.MemoryHash != f.commit.Attachment.Event.MemoryHash {
				t.Fatalf("bad recovery: %+v %v", p, err)
			}
		})
	}
}

func TestFirstTrackingCacheMatchesDurableEmptySlices(t *testing.T) {
	f := firstTrackingFixture(t)
	f.commit.ObservedSnapshots[0].Models = []string{}
	if err := f.store.CommitTrackingAttachment(context.Background(), f.commit); err != nil {
		t.Fatal(err)
	}
}

func TestFirstTrackingInitializerPreservesExistingHead(t *testing.T) {
	f := newTrackingFixture(t)
	ctx := context.Background()
	before, err := f.store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.InitializeHeadIfAbsent(ctx, domain.Ref{RepoID: before.RepoID, Kind: domain.RefHEAD, Name: "HEAD", Symbolic: "different"}); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.GetWorkingPosition(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("setup initializer replaced position", err)
	}
	// Explicit init/PutRef still has the existing mutating contract.
	if err = f.store.PutRef(ctx, domain.Ref{RepoID: before.RepoID, Kind: domain.RefHEAD, Name: "HEAD", Symbolic: "different"}); err != nil {
		t.Fatal(err)
	}
	changed, err := f.store.GetWorkingPosition(ctx)
	if err != nil || changed.Branch != "different" {
		t.Fatal("standalone initialization changed", err)
	}
}
