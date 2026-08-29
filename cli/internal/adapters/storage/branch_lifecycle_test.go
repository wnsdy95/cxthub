package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestFileStoreBranchLifecycleBlocksStaleResurrectionAndRestoresExplicitly(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	repoID := string(domain.HashContent([]byte("branch lifecycle repo")))
	target := domain.HashContent([]byte("branch lifecycle target"))
	branch := domain.Ref{Kind: domain.RefBranch, Name: "feature/archive", RepoID: repoID, Target: target}

	active, err := store.CreateBranchRef(ctx, branch)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	activeEvent, ok, err := domain.ParseBranchLifecycleRef(active)
	if err != nil || !ok || activeEvent.State != domain.BranchActive || activeEvent.Generation != 1 {
		t.Fatalf("active event = %+v, %v, %v", activeEvent, ok, err)
	}
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: domain.HeadRefName, RepoID: repoID, Symbolic: branch.Name}); err != nil {
		t.Fatal(err)
	}

	archived, err := store.ArchiveBranchRef(ctx, repoID, branch.Name)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	archiveEvent, ok, err := domain.ParseBranchLifecycleRef(archived)
	if err != nil || !ok || archiveEvent.State != domain.BranchArchived || archiveEvent.Generation != 2 {
		t.Fatalf("archive event = %+v, %v, %v", archiveEvent, ok, err)
	}
	if _, err := store.GetRef(ctx, repoID, domain.RefBranch, branch.Name); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("archived branch still active: %v", err)
	}
	if head, err := store.GetRef(ctx, repoID, domain.RefHEAD, domain.HeadRefName); err != nil || head.Symbolic != "" || head.Target != target {
		t.Fatalf("HEAD was not detached at archived target: %+v, %v", head, err)
	}
	if err := store.PutRef(ctx, branch); !errors.Is(err, domain.ErrBranchArchived) {
		t.Fatalf("stale PutRef resurrected branch: %v", err)
	}

	restored, err := store.CreateBranchRef(ctx, branch)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	restoreEvent, ok, err := domain.ParseBranchLifecycleRef(restored)
	if err != nil || !ok || restoreEvent.State != domain.BranchActive || restoreEvent.Generation != 3 {
		t.Fatalf("restore event = %+v, %v, %v", restoreEvent, ok, err)
	}
	if got, err := store.GetRef(ctx, repoID, domain.RefBranch, branch.Name); err != nil || got.Target != target {
		t.Fatalf("restored branch = %+v, %v", got, err)
	}
}

func TestFileStoreApplyArchivePreservesLiveOrAdvancedLocalBranch(t *testing.T) {
	ctx := context.Background()
	repoID := string(domain.HashContent([]byte("branch apply repo")))
	archivedTarget := domain.HashContent([]byte("archived target"))
	advancedTarget := domain.HashContent([]byte("advanced target"))

	for _, tc := range []struct {
		name          string
		localTarget   domain.ContentHash
		preserve      bool
		wantPreserved bool
	}{
		{name: "same inactive target is archived", localTarget: archivedTarget},
		{name: "same live Git branch wins", localTarget: archivedTarget, preserve: true, wantPreserved: true},
		{name: "advanced local context wins", localTarget: advancedTarget, wantPreserved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewFileStore(t.TempDir())
			branch := domain.Ref{Kind: domain.RefBranch, Name: "feature/shared", RepoID: repoID, Target: tc.localTarget}
			if _, err := store.CreateBranchRef(ctx, branch); err != nil {
				t.Fatal(err)
			}
			archive, err := domain.NewBranchLifecycleRef(repoID, branch.Name, archivedTarget, 2, domain.BranchArchived)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.ApplyBranchLifecycleRef(ctx, archive, tc.preserve); err != nil {
				t.Fatalf("apply: %v", err)
			}
			got, getErr := store.GetRef(ctx, repoID, domain.RefBranch, branch.Name)
			if !tc.wantPreserved {
				if !errors.Is(getErr, domain.ErrNotFound) {
					t.Fatalf("branch not archived: %+v, %v", got, getErr)
				}
				return
			}
			if getErr != nil || got.Target != tc.localTarget {
				t.Fatalf("live branch changed: %+v, %v", got, getErr)
			}
			refs, err := store.ListRefs(ctx, repoID)
			if err != nil {
				t.Fatal(err)
			}
			latest, ok, err := domain.LatestBranchLifecycle(refs, branch.Name)
			if err != nil || !ok || latest.State != domain.BranchActive || latest.Generation != 3 || latest.Target != tc.localTarget {
				t.Fatalf("compensating active event = %+v, %v, %v", latest, ok, err)
			}
		})
	}
}

func TestFileStorePutRefRepairsLifecycleBeforeMovingAdvancedArchiveResidue(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	repoID := string(domain.HashContent([]byte("branch lifecycle crash recovery repo")))
	archivedTarget := domain.HashContent([]byte("archived target"))
	advancedTarget := domain.HashContent([]byte("advanced target"))
	nextTarget := domain.HashContent([]byte("next target"))
	const branchName = "feature/recovered"

	// Simulate a crash/interleaving in which an archive event became durable,
	// while the physical branch had already advanced and no compensating active
	// event was written yet.
	archive, err := domain.NewBranchLifecycleRef(repoID, branchName, archivedTarget, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.putRefRaw(archive); err != nil {
		t.Fatal(err)
	}
	if err := store.putRefRaw(domain.Ref{Kind: domain.RefBranch, Name: branchName, RepoID: repoID, Target: advancedTarget}); err != nil {
		t.Fatal(err)
	}

	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: branchName, RepoID: repoID, Target: nextTarget}); err != nil {
		t.Fatalf("move advanced residue: %v", err)
	}
	got, err := store.GetRef(ctx, repoID, domain.RefBranch, branchName)
	if err != nil || got.Target != nextTarget {
		t.Fatalf("moved branch = %+v, %v", got, err)
	}
	refs, err := store.listRefsRaw(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok, err := domain.LatestBranchLifecycle(refs, branchName)
	if err != nil || !ok || latest.State != domain.BranchActive || latest.Generation != 2 || latest.Target != advancedTarget {
		t.Fatalf("recovery event = %+v, %v, %v", latest, ok, err)
	}
}

func TestFileStoreProjectsSymbolicHEADAfterInterruptedArchive(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	repoID := string(domain.HashContent([]byte("interrupted archive head repo")))
	target := domain.HashContent([]byte("interrupted archive head target"))
	const branchName = "feature/interrupted-head"
	if _, err := store.CreateBranchRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: branchName, RepoID: repoID, Target: target}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: domain.HeadRefName, RepoID: repoID, Symbolic: branchName}); err != nil {
		t.Fatal(err)
	}
	archive, err := domain.NewBranchLifecycleRef(repoID, branchName, target, 2, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	// Crash point: immutable archive is durable, but HEAD detach and branch
	// file removal have not run yet.
	if err := store.putRefRaw(archive); err != nil {
		t.Fatal(err)
	}
	head, err := store.GetRef(ctx, repoID, domain.RefHEAD, domain.HeadRefName)
	if err != nil || head.Symbolic != "" || head.Target != target {
		t.Fatalf("projected HEAD = %+v, %v", head, err)
	}
	if _, err := store.GetRef(ctx, repoID, domain.RefBranch, branchName); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("interrupted branch projection remains visible: %v", err)
	}
}

func TestFileStoreReconcilesInterruptedBranchLifecycleBeforePush(t *testing.T) {
	ctx := context.Background()
	repoID := string(domain.HashContent([]byte("reconcile interrupted lifecycle repo")))
	archivedTarget := domain.HashContent([]byte("reconcile archived target"))
	advancedTarget := domain.HashContent([]byte("reconcile advanced target"))

	t.Run("advanced branch gets durable active compensation", func(t *testing.T) {
		store := NewFileStore(t.TempDir())
		archive, err := domain.NewBranchLifecycleRef(repoID, "feature/advanced", archivedTarget, 1, domain.BranchArchived)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.putRefRaw(archive); err != nil {
			t.Fatal(err)
		}
		if err := store.putRefRaw(domain.Ref{Kind: domain.RefBranch, Name: "feature/advanced", RepoID: repoID, Target: advancedTarget}); err != nil {
			t.Fatal(err)
		}
		if err := store.ReconcileBranchLifecycleRefs(ctx, repoID); err != nil {
			t.Fatal(err)
		}
		refs, err := store.listRefsRaw(ctx, repoID)
		if err != nil {
			t.Fatal(err)
		}
		latest, ok, err := domain.LatestBranchLifecycle(refs, "feature/advanced")
		if err != nil || !ok || latest.State != domain.BranchActive || latest.Generation != 2 || latest.Target != advancedTarget {
			t.Fatalf("reconciled lifecycle = %+v, %v, %v", latest, ok, err)
		}
	})

	t.Run("equal stale branch is removed and symbolic HEAD detached", func(t *testing.T) {
		store := NewFileStore(t.TempDir())
		const branch = "feature/equal"
		archive, err := domain.NewBranchLifecycleRef(repoID, branch, archivedTarget, 1, domain.BranchArchived)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.putRefRaw(archive); err != nil {
			t.Fatal(err)
		}
		if err := store.putRefRaw(domain.Ref{Kind: domain.RefBranch, Name: branch, RepoID: repoID, Target: archivedTarget}); err != nil {
			t.Fatal(err)
		}
		if err := store.putRefRaw(domain.Ref{Kind: domain.RefHEAD, Name: domain.HeadRefName, RepoID: repoID, Symbolic: branch}); err != nil {
			t.Fatal(err)
		}
		if err := store.ReconcileBranchLifecycleRefs(ctx, repoID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.getRefRaw(ctx, repoID, domain.RefBranch, branch); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("stale raw branch remains: %v", err)
		}
		head, err := store.getRefRaw(ctx, repoID, domain.RefHEAD, domain.HeadRefName)
		if err != nil || head.Symbolic != "" || head.Target != archivedTarget {
			t.Fatalf("reconciled HEAD = %+v, %v", head, err)
		}
	})
}
