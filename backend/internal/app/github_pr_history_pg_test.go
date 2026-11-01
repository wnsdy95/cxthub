//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGGitHubPRHistoryPageAtomicityAndStaleReader(t *testing.T) {
	core, storage, _ := collaborationPG(t)
	fixture := makeTeamFixture(t, storage)
	ctx := systemTestContext()
	installationID := time.Now().UnixMicro()
	repo := domain.Repo{ID: hh(fmt.Sprintf("%s-%d", t.Name(), installationID)), RepositoryID: fixture.repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := storage.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	connection := domain.GitHubConnection{NamespaceID: fixture.organization.NamespaceID, Installation: domain.GitHubInstallation{ID: installationID, AccountID: 456, Kind: "Organization", Login: "acme"}, Enabled: true, Generation: 1, Status: "connected", CheckedAt: time.Now().UTC(), Repositories: []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}, Bindings: []domain.GitHubBinding{{RepositoryID: fixture.repository.ID, ContextRepoID: repo.ID, ExternalID: 91, GitOrigin: repo.GitRemoteURL}}}
	if err := storage.WithinIdentity(ctx, func(tx context.Context) error { return storage.PutGitHubConnection(tx, connection) }); err != nil {
		t.Fatal(err)
	}
	base := collaborationSnapshot(t, storage, repo.ID, "base")
	source := collaborationSnapshot(t, storage, repo.ID, "original source", base)
	if err := storage.CompareAndSwapRef(ctx, repo.ID, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		t.Fatal(err)
	}
	ordinary := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo.ID), BranchID: "feature", Branch: "feature", Kind: "birth", Source: base, Target: source, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(100, 0).UTC()}
	if err := core.RecordHistory(ctx, ordinary); err != nil {
		t.Fatal(err)
	}
	memory, err := core.PutMemoryDigest(ctx, repo.ID, domain.MemoryDigest{SnapshotID: source, Summary: "PINNED CONTRIBUTION MEMORY"})
	if err != nil {
		t.Fatal(err)
	}
	publication := prPublication(ordinary)
	publication.MemoryHash = memory
	if err := core.RecordHistory(ctx, publication); err != nil {
		t.Fatal(err)
	}
	position := domain.HistoryEvent{ID: strings.Repeat("8", 32), RepoID: string(repo.ID), BranchID: domain.LegacyContextBranchID(string(repo.ID), "main"), Branch: "main", Kind: "position", Source: base, Target: base, GitAfter: strings.Repeat("e", 40), CreatedAt: time.Unix(400, 0).UTC()}
	if err := core.RecordHistory(ctx, position); err != nil {
		t.Fatal(err)
	}
	gitReader := &scanReader{deltas: map[string]domain.GitCommitDelta{
		strings.Repeat("d", 40): {Commit: strings.Repeat("d", 40), Parents: []string{}, Complete: true, Changes: []domain.GitPathChange{}},
		strings.Repeat("e", 40): {Commit: strings.Repeat("e", 40), Parent: strings.Repeat("d", 40), Parents: []string{strings.Repeat("d", 40)}, Complete: true, Changes: []domain.GitPathChange{}},
	}}
	scans, err := NewGitScans(core, gitReader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scans.ObservePush(ctx, repo.GitRemoteURL, "refs/heads/main", "", position.GitAfter, false, "current-main"); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 4; attempt++ {
		if err := scans.Process(ctx, 100); err != nil {
			t.Fatal(err)
		}
		if _, err = storage.GetGitCommitTree(ctx, repo.ID, repo.GitRemoteURL, position.GitAfter); err != nil {
			continue
		}
		if _, err = storage.GetGitCommitTree(ctx, repo.ID, repo.GitRemoteURL, strings.Repeat("d", 40)); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("fixture Git evidence unavailable: %v", err)
	}
	pr := domain.PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("c", 40), MergeSHA: strings.Repeat("d", 40)}
	if _, err := core.SubmitPRPromotion(ctx, repo.ID, pr); err != nil {
		t.Fatal(err)
	}
	page := domain.GitHubPRHistoryPage{RepositoryID: 91, HeadRepositoryID: 91, PullRequestID: "PR_7", PullRequestDatabaseID: 701, Number: 7, BaseBranch: "main", HeadBranch: "feature", HeadSHA: pr.HeadSHA, MergeSHA: pr.MergeSHA, Merged: true, MergedAt: time.Unix(300, 0).UTC(), Rewrites: []domain.GitHubHeadRewrite{{ID: "event-1", Before: ordinary.GitAfter, After: strings.Repeat("b", 40), CreatedAt: time.Unix(200, 0).UTC()}}}
	remote := &githubHistoryRemote{GitHubApp: &githubFake{}, installationID: installationID, pages: map[string]domain.GitHubPRHistoryPage{"": page}}
	failing := &githubObservationRollbackPG{PostgresStore: storage, rejectRevision: true}
	core.meta = failing
	worker := NewGitHubConnections(fixture.identity, core, storage, remote)
	work := historyWork(t, worker, storage, repo)
	before, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.reconcilePRHistory(ctx, remote, failing, work); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("expected injected revision rollback: %v", err)
	}
	reserved, err := storage.GetPRHistoryCheckpoint(ctx, repo.ID, repo.GitRemoteURL, 91, 7)
	if err != nil || reserved.Version != 1 || reserved.PullRequestID != 0 || reserved.Cursor != "" || reserved.Complete {
		t.Fatalf("page advanced despite rollback: %+v, %v", reserved, err)
	}
	observations, err := storage.ListPRRefObservations(ctx, repo.ID, repo.GitRemoteURL, 91, 7, "", 101)
	if err != nil || len(observations) != 0 {
		t.Fatalf("observations leaked: %+v, %v", observations, err)
	}
	proofs, err := storage.ListPRParticipations(ctx, repo.ID, repo.GitRemoteURL, 7)
	if err != nil || len(proofs) != 0 {
		t.Fatalf("participation leaked: %+v, %v", proofs, err)
	}
	contributions, err := storage.ListPRContributions(ctx, repo.ID)
	if err != nil || len(contributions) != 0 {
		t.Fatalf("contribution leaked: %+v %v", contributions, err)
	}
	memories, err := storage.ListPRContributionMemories(ctx, repo.ID)
	if err != nil || len(memories) != 0 {
		t.Fatalf("memory pin leaked: %+v %v", memories, err)
	}
	job, err := storage.GetPRJob(ctx, repo.ID, domain.PRPromotionID(repo.ID, 7))
	if err != nil || job.State != "waiting" {
		t.Fatalf("job completion leaked: %+v %v", job, err)
	}
	after, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil || after != before {
		t.Fatalf("revision leaked: %+v -> %+v, %v", before, after, err)
	}
	failing.rejectRevision = false
	work = historyWork(t, worker, storage, repo)
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	otherCore := NewService(peer, peer, auth.NewTeamTokenAuth(), gitengine.NewEngine(peer), peer)
	other := NewGitHubConnections(fixture.identity, otherCore, peer, remote)
	started, release := make(chan struct{}, 2), make(chan struct{})
	reader := func() *githubHistoryRemote {
		return &githubHistoryRemote{GitHubApp: &githubFake{}, installationID: installationID, pages: remote.pages, during: func() { started <- struct{}{}; <-release }}
	}
	errorsSeen := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() { defer workers.Done(); errorsSeen <- worker.reconcilePRHistory(ctx, reader(), failing, work) }()
	go func() { defer workers.Done(); errorsSeen <- other.reconcilePRHistory(ctx, reader(), peer, work) }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		close(release)
		workers.Wait()
		t.Fatal("provider request did not start")
	}
	close(release)
	workers.Wait()
	close(errorsSeen)
	successes, conflicts := 0, 0
	for err := range errorsSeen {
		if err == nil {
			successes++
		} else if errors.Is(err, domain.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	checkpoint, err := storage.GetPRHistoryCheckpoint(ctx, repo.ID, repo.GitRemoteURL, 91, 7)
	if err != nil || checkpoint.Version != 3 || !checkpoint.Complete {
		t.Fatalf("cursor: %+v, %v", checkpoint, err)
	}
	proofs, err = storage.ListPRParticipations(ctx, repo.ID, repo.GitRemoteURL, 7)
	if err != nil || len(proofs) != 1 || proofs[0].Capture != source {
		t.Fatalf("proofs: %+v, %v", proofs, err)
	}
	contributions, err = storage.ListPRContributions(ctx, repo.ID)
	if err != nil || len(contributions) != 1 || contributions[0].Capture != source {
		t.Fatalf("contribution missing: %+v %v", contributions, err)
	}
	memories, err = storage.ListPRContributionMemories(ctx, repo.ID)
	if err != nil || len(memories) != 1 || memories[0].MemoryHash != memory {
		t.Fatalf("memory pin missing: %+v %v", memories, err)
	}
	job, err = storage.GetPRJob(ctx, repo.ID, domain.PRPromotionID(repo.ID, 7))
	if err != nil || job.State != "completed" {
		t.Fatalf("contribution job unfinished: %+v %v", job, err)
	}
	main, err := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
	if err != nil || main.Target != base {
		t.Fatalf("reconciliation moved main: %+v, %v", main, err)
	}
	assertCapturedGraphParityPG(t, core, storage, repo.ID, nil)
}
