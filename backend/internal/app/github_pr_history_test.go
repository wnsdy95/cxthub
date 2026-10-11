package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type githubHistoryRemote struct {
	outbound.GitHubApp
	pages          map[string]domain.GitHubPRHistoryPage
	calls          []string
	err            error
	during         func()
	installationID int64
}

func (remote *githubHistoryRemote) PullRequestHeadHistory(_ context.Context, installation int64, repository domain.GitHubRepository, number int, cursor string) (domain.GitHubPRHistoryPage, error) {
	remote.calls = append(remote.calls, cursor)
	if remote.during != nil {
		remote.during()
	}
	expectedInstallation := remote.installationID
	if expectedInstallation == 0 {
		expectedInstallation = 789
	}
	if installation != expectedInstallation || repository.ID != 91 || number != 7 {
		return domain.GitHubPRHistoryPage{}, domain.ErrIntegrity
	}
	return remote.pages[cursor], remote.err
}

func githubHistoryFixture(t *testing.T) (*GitHubConnections, *store.FSStore, *githubHistoryRemote, domain.Repo, domain.HistoryEvent) {
	t.Helper()
	worker, storage, repo := githubPRObservationFixture(t)
	ctx := systemTestContext()
	base := prSnapshot(t, storage, repo.ID, "base")
	source := prSnapshot(t, storage, repo.ID, "original before remote rebase", base)
	if err := storage.CompareAndSwapRef(ctx, repo.ID, domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		t.Fatal(err)
	}
	ordinary := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo.ID), BranchID: "feature", Branch: "feature", Kind: "birth", Source: base, Target: source, GitAfter: strings.Repeat("a", 40), WorktreeID: strings.Repeat("5", 32), CreatedAt: time.Unix(100, 0).UTC()}
	if err := worker.core.RecordHistory(ctx, ordinary); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("c", 40), MergeSHA: strings.Repeat("d", 40)}
	if _, err := worker.core.SubmitPRPromotion(ctx, repo.ID, pr); err != nil {
		t.Fatal(err)
	}
	page := domain.GitHubPRHistoryPage{RepositoryID: 91, HeadRepositoryID: 91, PullRequestID: "PR_7", PullRequestDatabaseID: 701, Number: 7, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("e", 40), MergeSHA: pr.MergeSHA, Merged: true, MergedAt: time.Unix(300, 0).UTC(), Rewrites: []domain.GitHubHeadRewrite{{ID: "event-1", Before: ordinary.GitAfter, After: strings.Repeat("b", 40), CreatedAt: time.Unix(200, 0).UTC()}}, EndCursor: "page-2", HasNextPage: true}
	last := page
	last.Rewrites = []domain.GitHubHeadRewrite{{ID: "event-2", Before: strings.Repeat("b", 40), After: pr.HeadSHA, CreatedAt: time.Unix(250, 0).UTC()}}
	last.HasNextPage, last.EndCursor = false, ""
	remote := &githubHistoryRemote{GitHubApp: worker.remote, pages: map[string]domain.GitHubPRHistoryPage{"": page, "page-2": last}}
	worker.remote = remote
	return worker, storage, remote, repo, ordinary
}

func historyWork(t *testing.T, worker *GitHubConnections, storage outbound.PRHistoryStore, repo domain.Repo) githubPRHistoryWork {
	t.Helper()
	ctx := systemTestContext()
	connections, err := worker.store.ListGitHubConnections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, connection := range connections {
		for _, binding := range connection.Bindings {
			if binding.ContextRepoID != repo.ID {
				continue
			}
			jobs, err := worker.core.ListPRPromotions(ctx, repo.ID)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("jobs: %+v, %v", jobs, err)
			}
			checkpoint, err := storage.GetPRHistoryCheckpoint(ctx, repo.ID, binding.GitOrigin, binding.ExternalID, jobs[0].PR.Number)
			if errors.Is(err, domain.ErrNotFound) {
				checkpoint = domain.PRHistoryCheckpoint{RepoID: repo.ID, GitOrigin: binding.GitOrigin, InstallationID: connection.Installation.ID, ProviderRepositoryID: binding.ExternalID, PR: jobs[0].PR}
			} else if err != nil {
				t.Fatal(err)
			}
			for _, remote := range connection.Repositories {
				if remote.ID == binding.ExternalID {
					return githubPRHistoryWork{connection: connection, binding: binding, remote: remote, checkpoint: checkpoint}
				}
			}
		}
	}
	t.Fatal("missing fixture binding")
	return githubPRHistoryWork{}
}

func TestGitHubPRHistoryReconciliationPreservesCaptureAndFindsLatePublications(t *testing.T) {
	worker, storage, remote, repo, ordinary := githubHistoryFixture(t)
	ctx := systemTestContext()
	publishPRSource(t, worker.core, ordinary)
	before, _ := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
	historyBefore, _ := worker.core.ListHistory(ctx, repo.ID)
	if err := worker.reconcilePRHistories(ctx, 1); err != nil {
		t.Fatal(err)
	}
	work := historyWork(t, worker, storage, repo)
	if work.checkpoint.Cursor != "page-2" || work.checkpoint.Version != 2 {
		t.Fatalf("page did not checkpoint: %+v", work.checkpoint)
	}
	worker = NewGitHubConnections(worker.id, worker.core, storage, remote)
	if err := worker.reconcilePRHistory(ctx, remote, storage, work); err != nil {
		t.Fatal(err)
	}
	proofs, err := storage.ListPRParticipations(ctx, repo.ID, repo.GitRemoteURL, 7)
	if err != nil || len(proofs) != 1 || proofs[0].OriginalHead != ordinary.GitAfter || proofs[0].Capture != ordinary.Target {
		t.Fatalf("original source proof: %+v, %v", proofs, err)
	}
	after, _ := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
	historyAfter, _ := worker.core.ListHistory(ctx, repo.ID)
	if before != after || !reflect.DeepEqual(historyBefore, historyAfter) {
		t.Fatal("association forged history or changed current selection")
	}
	late := ordinary
	late.ID, late.Kind, late.GitAfter = strings.Repeat("3", 32), "position", strings.Repeat("b", 40)
	late.Target = prSnapshot(t, storage, repo.ID, "late uploaded genuine capture", ordinary.Target)
	late.Source, late.CreatedAt = late.Target, time.Unix(240, 0).UTC()
	if err := worker.core.RecordHistory(ctx, late); err != nil {
		t.Fatal(err)
	}
	publishPRSource(t, worker.core, late)
	work = historyWork(t, worker, storage, repo)
	if !work.checkpoint.Complete {
		t.Fatal("provider traversal not complete")
	}
	if err := worker.reconcilePRHistory(ctx, remote, storage, work); err != nil {
		t.Fatal(err)
	}
	proofs, err = storage.ListPRParticipations(ctx, repo.ID, repo.GitRemoteURL, 7)
	if err != nil || len(proofs) != 3 || len(remote.calls) != 2 {
		t.Fatalf("late capture lost or needless provider reread: proofs=%+v calls=%v err=%v", proofs, remote.calls, err)
	}
	if err := worker.reconcilePRHistory(ctx, remote, storage, work); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale checkpoint was not fenced: %v", err)
	}
}

func TestGitHubPRHistoryRetriesWithoutAdvancingFailedPage(t *testing.T) {
	worker, storage, remote, repo, ordinary := githubHistoryFixture(t)
	ctx := systemTestContext()
	publishPRSource(t, worker.core, ordinary)
	remote.err = errors.New("provider unavailable")
	if err := worker.reconcilePRHistories(ctx, 1); err != nil {
		t.Fatal(err)
	}
	work := historyWork(t, worker, storage, repo)
	if work.checkpoint.Version != 1 || work.checkpoint.Cursor != "" || work.checkpoint.PullRequestID != 0 || work.checkpoint.NextAttempt.IsZero() {
		t.Fatalf("missing durable retry: %+v", work.checkpoint)
	}
	remote.err = nil
	if err := worker.reconcilePRHistory(ctx, remote, storage, work); err != nil {
		t.Fatal(err)
	}
	work = historyWork(t, worker, storage, repo)
	page := remote.pages["page-2"]
	page.PullRequestDatabaseID++
	remote.pages["page-2"] = page
	if err := worker.reconcilePRHistory(ctx, remote, storage, work); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("changed PR identity accepted: %v", err)
	}
	current := historyWork(t, worker, storage, repo).checkpoint
	if current.Cursor != "page-2" || current.PullRequestID != 701 || current.Complete {
		t.Fatalf("invalid page advanced evidence: %+v", current)
	}
}

func TestGitHubPRHistoryBindingRevocationFencesExternalRead(t *testing.T) {
	worker, storage, remote, repo, ordinary := githubHistoryFixture(t)
	ctx := systemTestContext()
	publishPRSource(t, worker.core, ordinary)
	work := historyWork(t, worker, storage, repo)
	remote.during = func() {
		connection := work.connection
		connection.Enabled = false
		connection.Generation++
		if err := storage.PutGitHubConnection(ctx, connection); err != nil {
			t.Fatal(err)
		}
	}
	if err := worker.reconcilePRHistory(ctx, remote, storage, work); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("revoked read committed: %v", err)
	}
	checkpoint, err := storage.GetPRHistoryCheckpoint(ctx, repo.ID, repo.GitRemoteURL, 91, 7)
	if err != nil || checkpoint.Version != 1 || checkpoint.PullRequestID != 0 || checkpoint.Complete || checkpoint.Cursor != "" {
		t.Fatalf("revoked read advanced reserved work: %+v, %v", checkpoint, err)
	}
	proofs, err := storage.ListPRParticipations(ctx, repo.ID, repo.GitRemoteURL, 7)
	if err != nil || len(proofs) != 0 {
		t.Fatalf("revoked participation persisted: %+v, %v", proofs, err)
	}
}

func TestGitHubPRHistoryLocalProofSurvivesTransientProviderFailure(t *testing.T) {
	for _, forbidden := range []bool{false, true} {
		t.Run(fmt.Sprint(forbidden), func(t *testing.T) {
			worker, storage, remote, repo, ordinary := githubHistoryFixture(t)
			ctx := systemTestContext()
			for page := 0; page < 2; page++ {
				if err := worker.reconcilePRHistory(ctx, remote, storage, historyWork(t, worker, storage, repo)); err != nil {
					t.Fatal(err)
				}
			}
			work := historyWork(t, worker, storage, repo)
			expired := work.checkpoint
			expired.HistoryCheckedAt = time.Now().UTC().Add(-time.Hour)
			expired.Version++
			if err := storage.SavePRHistoryCheckpoint(ctx, expired, work.checkpoint.Version); err != nil {
				t.Fatal(err)
			}
			publishPRSource(t, worker.core, ordinary)
			remote.err = errors.New("rate limited")
			if forbidden {
				remote.err = domain.ErrForbidden
			}
			if err := worker.reconcilePRHistory(ctx, remote, storage, historyWork(t, worker, storage, repo)); !errors.Is(err, remote.err) {
				t.Fatal(err)
			}
			proofs, err := storage.ListPRParticipations(ctx, repo.ID, repo.GitRemoteURL, 7)
			expected := 1
			if forbidden {
				expected = 0
			}
			if err != nil || len(proofs) != expected {
				t.Fatalf("provider failure policy: %+v, %v", proofs, err)
			}
		})
	}
}

func TestGitHubPRHistoryOlderThanLatestHundredJobsStillReconciles(t *testing.T) {
	worker, storage, _, repo, ordinary := githubHistoryFixture(t)
	ctx := systemTestContext()
	publishPRSource(t, worker.core, ordinary)
	work := historyWork(t, worker, storage, repo)
	for number := 100; number < 201; number++ {
		pr := work.checkpoint.PR
		pr.Number = number
		created := time.Now().UTC().Add(time.Duration(number) * time.Second)
		job := domain.PRPromotionJob{ID: domain.PRPromotionID(repo.ID, number), RepoID: repo.ID, PR: pr, GitOrigin: normalizeGitURL(repo.GitRemoteURL), BaseBranchID: domain.LegacyContextBranchID(string(repo.ID), "main"), State: "completed", CreatedAt: created, UpdatedAt: created, NextAttempt: created}
		if _, err := storage.EnqueuePRJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		checkpoint := work.checkpoint
		checkpoint.PR, checkpoint.Version = pr, 1
		checkpoint.NextAttempt = time.Now().UTC().Add(time.Hour)
		if err := storage.SavePRHistoryCheckpoint(ctx, checkpoint, 0); err != nil {
			t.Fatal(err)
		}
	}
	recent, err := storage.ListPRJobs(ctx, repo.ID)
	if err != nil || len(recent) != 100 {
		t.Fatalf("recent jobs fixture: %d, %v", len(recent), err)
	}
	for _, job := range recent {
		if job.PR.Number == 7 {
			t.Fatal("old job unexpectedly in UI's latest hundred")
		}
	}
	if err := worker.reconcilePRHistories(ctx, 1); err != nil {
		t.Fatal(err)
	}
	proofs, err := storage.ListPRParticipations(ctx, repo.ID, repo.GitRemoteURL, 7)
	if err != nil || len(proofs) != 1 {
		t.Fatalf("old source starved: %+v, %v", proofs, err)
	}
}

func TestGitHubPRHistoryBoundsMatchingAndExcludesPostMergeRewrites(t *testing.T) {
	worker, storage, remote, repo, ordinary := githubHistoryFixture(t)
	ctx := systemTestContext()
	page := remote.pages[""]
	page.HasNextPage, page.EndCursor = false, ""
	page.Rewrites = append(page.Rewrites, domain.GitHubHeadRewrite{ID: "after-merge-reuse", Before: strings.Repeat("e", 40), After: strings.Repeat("f", 40), CreatedAt: page.MergedAt.Add(time.Second)})
	remote.pages[""] = page
	for number := 1; number <= 205; number++ {
		publication := prPublication(ordinary)
		publication.ID = fmt.Sprintf("%032x", number+100)
		if err := storage.ApplyHistoryEvent(ctx, publication); err != nil {
			t.Fatal(err)
		}
	}
	for _, count := range []int{100, 200, 205} {
		if err := worker.reconcilePRHistory(ctx, remote, storage, historyWork(t, worker, storage, repo)); err != nil {
			t.Fatal(err)
		}
		proofs, err := storage.ListPRParticipations(ctx, repo.ID, repo.GitRemoteURL, 7)
		if err != nil || len(proofs) != count {
			t.Fatalf("bounded progress: %d, expected %d: %v", len(proofs), count, err)
		}
		worker = NewGitHubConnections(worker.id, worker.core, storage, remote)
	}
	observations, err := storage.ListPRRefObservations(ctx, repo.ID, repo.GitRemoteURL, 91, 7, "", 101)
	if err != nil || len(observations) != 1 || observations[0].PullRequest.ProviderEventID != "event-1" || len(remote.calls) != 1 {
		t.Fatalf("post-merge evidence used or checkpoint lost: %+v, %v", observations, err)
	}
	checkpoint := historyWork(t, worker, storage, repo).checkpoint
	if checkpoint.MatchCursor != "" || checkpoint.EvidenceCursor != "" {
		t.Fatalf("matching did not finish: %+v", checkpoint)
	}
}

func TestGitHubPRHistoryFailedJobsDoNotMonopolizeHealthyWork(t *testing.T) {
	worker, storage, remote, repo, ordinary := githubHistoryFixture(t)
	ctx := systemTestContext()
	publishPRSource(t, worker.core, ordinary)
	work := historyWork(t, worker, storage, repo)
	queued := 0
	for number := 100; queued < 4; number++ {
		id := domain.PRPromotionID(repo.ID, number)
		if id > domain.PRPromotionID(repo.ID, 7) {
			continue
		}
		pr := work.checkpoint.PR
		pr.Number = number
		now := time.Now().UTC().Add(-time.Hour)
		job := domain.PRPromotionJob{ID: id, RepoID: repo.ID, PR: pr, GitOrigin: normalizeGitURL(repo.GitRemoteURL), BaseBranchID: domain.LegacyContextBranchID(string(repo.ID), "main"), State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
		if _, err := storage.EnqueuePRJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		queued++
	}
	if err := worker.reconcilePRHistories(ctx, 4); err != nil {
		t.Fatal(err)
	}
	proofs, err := storage.ListPRParticipations(ctx, repo.ID, repo.GitRemoteURL, 7)
	if err != nil || len(proofs) != 1 || len(remote.calls) != 5 {
		t.Fatalf("failed jobs starved healthy work: proofs=%+v calls=%d err=%v", proofs, len(remote.calls), err)
	}
	if err := worker.reconcilePRHistories(ctx, 4); err != nil || len(remote.calls) != 5 {
		t.Fatalf("failed jobs retry without delay: calls=%d, %v", len(remote.calls), err)
	}
}

func TestGitHubPRHistoryResumptionPrecedesNewDiscovery(t *testing.T) {
	worker, storage, remote, repo, ordinary := githubHistoryFixture(t)
	ctx := systemTestContext()
	publishPRSource(t, worker.core, ordinary)
	work := historyWork(t, worker, storage, repo)
	checkpoint := work.checkpoint
	checkpoint.Version, checkpoint.NextAttempt = 1, time.Now().UTC().Add(-time.Minute)
	if err := storage.SavePRHistoryCheckpoint(ctx, checkpoint, 0); err != nil {
		t.Fatal(err)
	}
	for number := 100; number < 110; number++ {
		pr := work.checkpoint.PR
		pr.Number = number
		now := time.Now().UTC()
		job := domain.PRPromotionJob{ID: domain.PRPromotionID(repo.ID, number), RepoID: repo.ID, PR: pr, GitOrigin: normalizeGitURL(repo.GitRemoteURL), BaseBranchID: domain.LegacyContextBranchID(string(repo.ID), "main"), State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
		if _, err := storage.EnqueuePRJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	if err := worker.reconcilePRHistories(ctx, 1); err != nil {
		t.Fatal(err)
	}
	proofs, err := storage.ListPRParticipations(ctx, repo.ID, repo.GitRemoteURL, 7)
	if err != nil || len(proofs) != 1 || len(remote.calls) != 1 {
		t.Fatalf("new discovery starved resumption: %+v calls=%d err=%v", proofs, len(remote.calls), err)
	}
}
