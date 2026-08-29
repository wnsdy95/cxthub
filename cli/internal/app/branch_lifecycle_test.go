package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type branchLifecycleGit struct{ repo domain.Repo }

func (g branchLifecycleGit) CurrentRepo(context.Context, string) (domain.Repo, error) {
	return g.repo, nil
}

type branchLifecycleRemote struct {
	outbound.RemoteSync
	refs []domain.Ref
}

func (r branchLifecycleRemote) Pull(context.Context, string, map[domain.ContentHash]domain.ContentHash, []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	return nil, nil, r.refs, nil
}

type branchInventoryGit struct {
	outbound.GitContext
	branches []string
	err      error
}

func (g branchInventoryGit) LocalBranches(context.Context, string) ([]string, error) {
	if g.err != nil {
		return nil, g.err
	}
	return append([]string(nil), g.branches...), nil
}

func TestPullArchivePrunesOnlyBranchAbsentFromLocalGit(t *testing.T) {
	ctx := context.Background()
	repoID := string(domain.HashContent([]byte("pull archive repo")))
	target := domain.HashContent([]byte("pull archive target"))
	archive, err := domain.NewBranchLifecycleRef(repoID, "feature/shared", target, 2, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name        string
		gitBranches []string
		wantActive  bool
	}{
		{name: "deleted Git branch pruned"},
		{name: "live local Git branch preserved", gitBranches: []string{"feature/shared"}, wantActive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := storage.NewFileStore(t.TempDir())
			if err := store.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repoID, DocHash: target}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreateBranchRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "feature/shared", RepoID: repoID, Target: target}); err != nil {
				t.Fatal(err)
			}
			// Old servers preserve the lifecycle tag but cannot remove their
			// branch projection. Pull must treat that exact pointer as shadowed.
			legacyBranch := domain.Ref{Kind: domain.RefBranch, Name: "feature/shared", RepoID: repoID, Target: target}
			svc := NewSyncRepoService(store, branchLifecycleRemote{refs: []domain.Ref{archive, legacyBranch}}, branchInventoryGit{branches: tc.gitBranches})
			if _, err := svc.Pull(ctx, inbound.SyncInput{RepoID: repoID, Cwd: "/repo"}); err != nil {
				t.Fatal(err)
			}
			ref, err := store.GetRef(ctx, repoID, domain.RefBranch, "feature/shared")
			if !tc.wantActive {
				if !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("stale branch remains: %+v, %v", ref, err)
				}
				return
			}
			if err != nil || ref.Target != target {
				t.Fatalf("live branch lost: %+v, %v", ref, err)
			}
			refs, err := store.ListRefs(ctx, repoID)
			if err != nil {
				t.Fatal(err)
			}
			latest, ok, err := domain.LatestBranchLifecycle(refs, "feature/shared")
			if err != nil || !ok || latest.State != domain.BranchActive || latest.Generation != 3 {
				t.Fatalf("live compensation = %+v, %v, %v", latest, ok, err)
			}
		})
	}
}

func TestPullRepairsAdvancedRemoteBranchMissingLifecycleCompensation(t *testing.T) {
	ctx := context.Background()
	repoID := string(domain.HashContent([]byte("pull lifecycle crash recovery repo")))
	archivedTarget := domain.HashContent([]byte("pull lifecycle archived target"))
	advancedTarget := domain.HashContent([]byte("pull lifecycle advanced target"))
	const branchName = "feature/recovered-remote"
	archive, err := domain.NewBranchLifecycleRef(repoID, branchName, archivedTarget, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	branch := domain.Ref{Kind: domain.RefBranch, Name: branchName, RepoID: repoID, Target: advancedTarget}

	store := storage.NewFileStore(t.TempDir())
	for _, target := range []domain.ContentHash{archivedTarget, advancedTarget} {
		if err := store.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repoID, DocHash: target, Branch: branchName}); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewSyncRepoService(store, branchLifecycleRemote{refs: []domain.Ref{archive, branch}}, branchInventoryGit{})
	if _, err := svc.Pull(ctx, inbound.SyncInput{RepoID: repoID, Cwd: "/repo"}); err != nil {
		t.Fatalf("pull advanced archive residue: %v", err)
	}
	got, err := store.GetRef(ctx, repoID, domain.RefBranch, branchName)
	if err != nil || got.Target != advancedTarget {
		t.Fatalf("recovered branch = %+v, %v", got, err)
	}
	refs, err := store.ListRefs(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok, err := domain.LatestBranchLifecycle(refs, branchName)
	if err != nil || !ok || latest.State != domain.BranchActive || latest.Generation != 2 || latest.Target != advancedTarget {
		t.Fatalf("pull recovery event = %+v, %v, %v", latest, ok, err)
	}
}

func TestPullDoesNotReactivateArchivesWhenGitInventoryFails(t *testing.T) {
	ctx := context.Background()
	repoID := string(domain.HashContent([]byte("pull archive inventory failure repo")))
	target := domain.HashContent([]byte("pull archive inventory failure target"))
	const branchName = "feature/uncertain"
	store := storage.NewFileStore(t.TempDir())
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repoID, DocHash: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBranchRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: branchName, RepoID: repoID, Target: target}); err != nil {
		t.Fatal(err)
	}
	archive, err := domain.NewBranchLifecycleRef(repoID, branchName, target, 2, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	inventoryErr := errors.New("injected Git inventory failure")
	svc := NewSyncRepoService(store, branchLifecycleRemote{refs: []domain.Ref{archive}}, branchInventoryGit{err: inventoryErr})
	if _, err := svc.Pull(ctx, inbound.SyncInput{RepoID: repoID, Cwd: "/repo"}); !errors.Is(err, inventoryErr) {
		t.Fatalf("pull error = %v, want inventory failure", err)
	}
	ref, err := store.GetRef(ctx, repoID, domain.RefBranch, branchName)
	if err != nil || ref.Target != target {
		t.Fatalf("branch changed after uncertain inventory: %+v, %v", ref, err)
	}
	refs, err := store.ListRefs(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok, err := domain.LatestBranchLifecycle(refs, branchName)
	if err != nil || !ok || latest.State != domain.BranchActive || latest.Generation != 1 {
		t.Fatalf("archive was applied despite inventory failure: %+v, %v, %v", latest, ok, err)
	}
}

func TestOrderRefsForPushPublishesLifecycleBeforeBranchProjection(t *testing.T) {
	repoID := string(domain.HashContent([]byte("push lifecycle order repo")))
	target := domain.HashContent([]byte("push lifecycle order target"))
	active, err := domain.NewBranchLifecycleRef(repoID, "feature/order", target, 1, domain.BranchActive)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := domain.NewBranchLifecycleRef(repoID, "feature/old", target, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	branch := domain.Ref{Kind: domain.RefBranch, Name: "feature/order", RepoID: repoID, Target: target}
	ordinaryTag := domain.Ref{Kind: domain.RefTag, Name: "v1", RepoID: repoID, Target: target}
	ordered := orderRefsForPush([]domain.Ref{branch, ordinaryTag, archive, active})
	for i := 0; i < 2; i++ {
		if _, ok, err := domain.ParseBranchLifecycleRef(ordered[i]); err != nil || !ok {
			t.Fatalf("ref %d is not lifecycle: %+v, %v", i, ordered[i], err)
		}
	}
	if ordered[2].Kind == domain.RefTag && ordered[2].Name == archive.Name || ordered[2].Name == active.Name {
		t.Fatalf("lifecycle event remained after ordinary refs: %+v", ordered)
	}
}

func TestTagListHidesInternalBranchLifecycleEvents(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: string(domain.HashContent([]byte("tag list lifecycle repo")))}
	target := domain.HashContent([]byte("tag list lifecycle target"))
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefTag, Name: "release/v1", RepoID: repo.ID, Target: target}); err != nil {
		t.Fatal(err)
	}
	event, err := domain.NewBranchLifecycleRef(repo.ID, "feature/hidden", target, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, event); err != nil {
		t.Fatal(err)
	}
	tags, err := NewTagService(branchLifecycleGit{repo: repo}, store).Tags(ctx, "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Name != "release/v1" {
		t.Fatalf("public tags = %+v", tags)
	}
	if _, err := NewTagService(branchLifecycleGit{repo: repo}, store).Tag(ctx, inbound.TagInput{Name: event.Name}); !errors.Is(err, domain.ErrInvalidRef) {
		t.Fatalf("reserved lifecycle namespace accepted as user tag: %v", err)
	}
}

func (g branchLifecycleGit) CurrentBranch(context.Context, string) (string, error) {
	return "main", nil
}

func TestBranchLifecycleServiceArchivesPointerWithoutDeletingSnapshot(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: string(domain.HashContent([]byte("archive service repo")))}
	target := domain.HashContent([]byte("archive service target"))
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repo.ID, DocHash: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBranchRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "feature/done", RepoID: repo.ID, Target: target}); err != nil {
		t.Fatal(err)
	}

	svc := NewBranchLifecycleService(branchLifecycleGit{repo: repo}, store)
	out, err := svc.Archive(ctx, inbound.BranchArchiveInput{Cwd: "/repo", Branch: "feature/done"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Branch != "feature/done" || out.Target != target || out.Event.Kind != domain.RefTag {
		t.Fatalf("archive output = %+v", out)
	}
	if _, err := store.GetRef(ctx, repo.ID, domain.RefBranch, out.Branch); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("active pointer remains: %v", err)
	}
	if _, err := store.GetSnapshot(ctx, target); err != nil {
		t.Fatalf("snapshot was deleted: %v", err)
	}
}

func TestBranchLifecycleServiceRenamePreservesOverwrittenDestination(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: string(domain.HashContent([]byte("rename service repo")))}
	sourceTarget := domain.HashContent([]byte("rename source target"))
	destinationTarget := domain.HashContent([]byte("rename overwritten destination target"))
	for _, target := range []domain.ContentHash{sourceTarget, destinationTarget} {
		if err := store.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repo.ID, DocHash: target}); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]domain.ContentHash{
		"feature/old": sourceTarget,
		"feature/new": destinationTarget,
	} {
		if _, err := store.CreateBranchRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: name, RepoID: repo.ID, Target: target}); err != nil {
			t.Fatal(err)
		}
	}

	svc := NewBranchLifecycleService(branchLifecycleGit{repo: repo}, store)
	out, err := svc.Rename(ctx, inbound.BranchRenameInput{Cwd: "/repo", From: "feature/old", To: "feature/new"})
	if err != nil {
		t.Fatal(err)
	}
	if out.From != "feature/old" || out.To != "feature/new" || out.Target != sourceTarget {
		t.Fatalf("rename output = %+v", out)
	}
	if _, err := store.GetRef(ctx, repo.ID, domain.RefBranch, "feature/old"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("source pointer remains: %v", err)
	}
	if ref, err := store.GetRef(ctx, repo.ID, domain.RefBranch, "feature/new"); err != nil || ref.Target != sourceTarget {
		t.Fatalf("destination ref = %+v, %v", ref, err)
	}
	for _, target := range []domain.ContentHash{sourceTarget, destinationTarget} {
		if _, err := store.GetSnapshot(ctx, target); err != nil {
			t.Fatalf("snapshot %s was lost: %v", target, err)
		}
	}
	refs, err := store.ListRefs(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldState, ok, err := domain.LatestBranchLifecycle(refs, "feature/old")
	if err != nil || !ok || oldState.State != domain.BranchArchived || oldState.Target != sourceTarget {
		t.Fatalf("old lifecycle = %+v, %v, %v", oldState, ok, err)
	}
	newState, ok, err := domain.LatestBranchLifecycle(refs, "feature/new")
	if err != nil || !ok || newState.State != domain.BranchActive || newState.Target != sourceTarget {
		t.Fatalf("new lifecycle = %+v, %v, %v", newState, ok, err)
	}
	archivedDestination := false
	for _, ref := range refs {
		event, lifecycle, parseErr := domain.ParseBranchLifecycleRef(ref)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if lifecycle && event.Branch == "feature/new" && event.State == domain.BranchArchived && event.Target == destinationTarget {
			archivedDestination = true
		}
	}
	if !archivedDestination {
		t.Fatal("overwritten destination context was not archived")
	}
	if _, err := svc.Rename(ctx, inbound.BranchRenameInput{Cwd: "/repo", From: "feature/old", To: "feature/new"}); err != nil {
		t.Fatalf("idempotent rename retry failed: %v", err)
	}
}

func TestForkRecordsActiveBranchLifecycleEvent(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repoID := string(domain.HashContent([]byte("fork lifecycle repo")))
	target := domain.HashContent([]byte("fork lifecycle target"))
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repoID, DocHash: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewForkSessionService(store).Fork(ctx, inbound.ForkInput{RepoID: repoID, FromSnapshot: target, NewBranch: "feature/new"}); err != nil {
		t.Fatal(err)
	}
	refs, err := store.ListRefs(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok, err := domain.LatestBranchLifecycle(refs, "feature/new")
	if err != nil || !ok || latest.State != domain.BranchActive || latest.Target != target {
		t.Fatalf("fork lifecycle = %+v, %v, %v", latest, ok, err)
	}
}

type branchLifecycleLoad struct{}

func (branchLifecycleLoad) Load(_ context.Context, in inbound.LoadInput) (inbound.LoadOutput, error) {
	return inbound.LoadOutput{Fidelity: domain.FidelityFull}, nil
}

type branchLifecycleFailLoad struct{}

func (branchLifecycleFailLoad) Load(context.Context, inbound.LoadInput) (inbound.LoadOutput, error) {
	return inbound.LoadOutput{}, errors.New("materialization failed")
}

func TestCheckoutByArchivedBranchNameRestoresActiveProjection(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repoID := string(domain.HashContent([]byte("checkout archived repo")))
	target := domain.HashContent([]byte("checkout archived target"))
	branch := domain.Ref{Kind: domain.RefBranch, Name: "feature/restore", RepoID: repoID, Target: target}
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repoID, DocHash: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBranchRef(ctx, branch); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ArchiveBranchRef(ctx, repoID, branch.Name); err != nil {
		t.Fatal(err)
	}

	checkout := NewCheckoutSessionService(NewForkSessionService(store), branchLifecycleLoad{}, store)
	out, err := checkout.Checkout(ctx, inbound.CheckoutInput{RepoID: repoID, From: branch.Name})
	if err != nil {
		t.Fatal(err)
	}
	if out.Branch != branch.Name || out.Head != target {
		t.Fatalf("checkout output = %+v", out)
	}
	if !out.ActivatedBranch {
		t.Fatalf("checkout did not report lifecycle activation: %+v", out)
	}
	if got, err := store.GetRef(ctx, repoID, domain.RefBranch, branch.Name); err != nil || got.Target != target {
		t.Fatalf("restored ref = %+v, %v", got, err)
	}
	refs, err := store.ListRefs(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok, err := domain.LatestBranchLifecycle(refs, branch.Name)
	if err != nil || !ok || latest.State != domain.BranchActive || latest.Generation != 3 {
		t.Fatalf("restore lifecycle = %+v, %v, %v", latest, ok, err)
	}
}

func TestCheckoutFailureLeavesArchivedBranchInactive(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repoID := string(domain.HashContent([]byte("failed archived checkout repo")))
	target := domain.HashContent([]byte("failed archived checkout target"))
	branch := domain.Ref{Kind: domain.RefBranch, Name: "feature/still-archived", RepoID: repoID, Target: target}
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repoID, DocHash: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBranchRef(ctx, branch); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ArchiveBranchRef(ctx, repoID, branch.Name); err != nil {
		t.Fatal(err)
	}

	checkout := NewCheckoutSessionService(NewForkSessionService(store), branchLifecycleFailLoad{}, store)
	if _, err := checkout.Checkout(ctx, inbound.CheckoutInput{RepoID: repoID, From: branch.Name}); err == nil {
		t.Fatal("checkout unexpectedly succeeded")
	}
	if _, err := store.GetRef(ctx, repoID, domain.RefBranch, branch.Name); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed checkout activated branch: %v", err)
	}
	refs, err := store.ListRefs(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok, err := domain.LatestBranchLifecycle(refs, branch.Name)
	if err != nil || !ok || latest.State != domain.BranchArchived || latest.Generation != 2 {
		t.Fatalf("failed checkout lifecycle = %+v, %v, %v", latest, ok, err)
	}
}
