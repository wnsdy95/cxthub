package store

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestFSStoreBranchLifecycleBlocksStaleResurrection(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := rlHash('4')
	target := rlHash('a')
	branch := domain.Ref{Kind: domain.RefBranch, Name: "feature/archive", RepoID: repo, Target: target}
	if err := st.CompareAndSwapRef(ctx, repo, branch, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefHead, Name: domain.HeadRefName, RepoID: repo, Symbolic: branch.Name}, ""); err != nil {
		t.Fatal(err)
	}
	archive, err := domain.NewBranchLifecycleRef(repo, branch.Name, target, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyBranchLifecycleRef(ctx, repo, archive); err != nil {
		t.Fatalf("apply archive: %v", err)
	}
	if _, err := st.GetRef(ctx, repo, domain.RefBranch, branch.Name); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("archived branch still active: %v", err)
	}
	if head, err := st.GetRef(ctx, repo, domain.RefHead, domain.HeadRefName); err != nil || head.Symbolic != "" || head.Target != target {
		t.Fatalf("HEAD was not detached at archived target: %+v, %v", head, err)
	}
	reflog, err := st.ReadReflog(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	archiveLogged, removalLogged := false, false
	for _, entry := range reflog {
		archiveLogged = archiveLogged || (entry.Kind == domain.RefTag && entry.Name == archive.Name && entry.New == target)
		removalLogged = removalLogged || (entry.Kind == domain.RefBranch && entry.Name == branch.Name && entry.Old == target && entry.New == "")
	}
	if !archiveLogged || !removalLogged {
		t.Fatalf("archive reflog missing tag/removal audit: %+v", reflog)
	}
	// Simulate a process crash after the immutable archive tag was written but
	// before the old branch file was removed.
	if err := writeAtomic(st.refFile(repo, domain.RefBranch, branch.Name), []byte(string(target)+"\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetRef(ctx, repo, domain.RefBranch, branch.Name); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("crash residue leaked through GetRef: %v", err)
	}
	refs, err := st.ListRefs(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if ref.Kind == domain.RefBranch && ref.Name == branch.Name {
			t.Fatalf("crash residue leaked into logical refs: %+v", ref)
		}
	}
	if err := st.CompareAndSwapRef(ctx, repo, branch, ""); !errors.Is(err, domain.ErrBranchArchived) {
		t.Fatalf("stale branch resurrected: %v", err)
	}
	if err := os.Remove(st.refFile(repo, domain.RefBranch, branch.Name)); err != nil {
		t.Fatal(err)
	}

	active, err := domain.NewBranchLifecycleRef(repo, branch.Name, target, 2, domain.BranchActive)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyBranchLifecycleRef(ctx, repo, active); err != nil {
		t.Fatalf("apply active: %v", err)
	}
	if err := st.CompareAndSwapRef(ctx, repo, branch, ""); err != nil {
		t.Fatalf("explicit restore rejected: %v", err)
	}
}

func TestFSStoreArchivePreservesAdvancedBranchWithCompensatingEvent(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := rlHash('5')
	oldTarget, advancedTarget := rlHash('a'), rlHash('b')
	branch := domain.Ref{Kind: domain.RefBranch, Name: "feature/live", RepoID: repo, Target: advancedTarget}
	if err := st.CompareAndSwapRef(ctx, repo, branch, ""); err != nil {
		t.Fatal(err)
	}
	archive, err := domain.NewBranchLifecycleRef(repo, branch.Name, oldTarget, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyBranchLifecycleRef(ctx, repo, archive); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetRef(ctx, repo, domain.RefBranch, branch.Name); err != nil || got.Target != advancedTarget {
		t.Fatalf("advanced branch changed: %+v, %v", got, err)
	}
	refs, err := st.ListRefs(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok, err := domain.LatestBranchLifecycle(refs, branch.Name)
	if err != nil || !ok || latest.State != domain.BranchActive || latest.Generation != 2 || latest.Target != advancedTarget {
		t.Fatalf("compensating event = %+v, %v, %v", latest, ok, err)
	}
}

func TestFSStoreCASRepairsLifecycleBeforeMovingAdvancedArchiveResidue(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := rlHash('6')
	archivedTarget, advancedTarget, nextTarget := rlHash('a'), rlHash('b'), rlHash('c')
	const branchName = "feature/recovered"

	// Simulate a crash/interleaving in which the immutable archive event and an
	// already-advanced physical branch exist without the active compensation
	// normally written by ApplyBranchLifecycleRef.
	archive, err := domain.NewBranchLifecycleRef(repo, branchName, archivedTarget, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(st.refFile(repo, domain.RefTag, archive.Name), []byte(string(archive.Target)+"\n")); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(st.refFile(repo, domain.RefBranch, branchName), []byte(string(advancedTarget)+"\n")); err != nil {
		t.Fatal(err)
	}

	next := domain.Ref{Kind: domain.RefBranch, Name: branchName, RepoID: repo, Target: nextTarget}
	if err := st.CompareAndSwapRef(ctx, repo, next, advancedTarget); err != nil {
		t.Fatalf("move advanced residue: %v", err)
	}
	got, err := st.GetRef(ctx, repo, domain.RefBranch, branchName)
	if err != nil || got.Target != nextTarget {
		t.Fatalf("moved branch = %+v, %v", got, err)
	}
	refs, err := st.listRefsRaw(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok, err := domain.LatestBranchLifecycle(refs, branchName)
	if err != nil || !ok || latest.State != domain.BranchActive || latest.Generation != 2 || latest.Target != advancedTarget {
		t.Fatalf("recovery event = %+v, %v, %v", latest, ok, err)
	}
}

func TestFSStoreProjectsSymbolicHEADAfterInterruptedArchive(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo, target := rlHash('7'), rlHash('d')
	const branchName = "feature/interrupted-head"
	branch := domain.Ref{Kind: domain.RefBranch, Name: branchName, RepoID: repo, Target: target}
	if err := st.CompareAndSwapRef(ctx, repo, branch, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefHead, Name: domain.HeadRefName, RepoID: repo, Symbolic: branchName}, ""); err != nil {
		t.Fatal(err)
	}
	archive, err := domain.NewBranchLifecycleRef(repo, branchName, target, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(st.refFile(repo, domain.RefTag, archive.Name), []byte(string(archive.Target)+"\n")); err != nil {
		t.Fatal(err)
	}
	head, err := st.GetRef(ctx, repo, domain.RefHead, domain.HeadRefName)
	if err != nil || head.Symbolic != "" || head.Target != target {
		t.Fatalf("projected HEAD = %+v, %v", head, err)
	}
	if _, err := st.GetRef(ctx, repo, domain.RefBranch, branchName); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("interrupted branch projection remains visible: %v", err)
	}
}
