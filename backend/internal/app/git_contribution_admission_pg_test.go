//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type admissionPGFixture struct {
	worker        *GitHubConnections
	storage       *store.PostgresStore
	repo          domain.Repo
	frozen        frozenGitAdmission
	prepared      domain.GitContributionAdmission
	claim         domain.GitHeadScan
	observations  []domain.GitRefObservation
	authorization gitAdmissionAuthorization
	dsn           string
}

func newAdmissionPGFixture(t *testing.T) admissionPGFixture {
	t.Helper()
	ctx := systemTestContext()
	dsn := a11IsolatedPGDSN(t, ctx)
	worker, storage, repo := providerObservationPGFixtureAt(t, dsn)
	base := collaborationSnapshot(t, storage, repo.ID, "admission base")
	capture := collaborationSnapshot(t, storage, repo.ID, "admission source", base)
	destinationID := domain.LegacyContextBranchID(string(repo.ID), "main")
	for _, ref := range []domain.Ref{
		{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", BranchID: destinationID, Target: base},
		{RepoID: repo.ID, Kind: domain.RefBranch, Name: "feature", BranchID: "feature", Target: capture},
	} {
		if err := storage.CompareAndSwapRef(ctx, repo.ID, ref, ""); err != nil {
			t.Fatal(err)
		}
	}
	ordinary := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo.ID), BranchID: "feature", Branch: "feature", Kind: "birth", Source: base, Target: capture, GitAfter: strings.Repeat("a", 40), WorktreeID: strings.Repeat("5", 32), CreatedAt: time.Unix(100, 0).UTC()}
	if err := worker.core.RecordHistory(ctx, ordinary); err != nil {
		t.Fatal(err)
	}
	publication := prPublication(ordinary)
	if err := worker.core.RecordHistory(ctx, publication); err != nil {
		t.Fatal(err)
	}
	frozen, err := worker.core.freezeGitAdmission(ctx, repo.ID, publication.ID, ordinary.GitAfter, "main", destinationID)
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := authorizeGitAdmission(ctx, worker, repo.ID)
	if err != nil {
		t.Fatal("provider binding unavailable", err)
	}
	identity := authorization.provider
	claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	baseline, root := strings.Repeat("b", 40), strings.Repeat("c", 40)
	observations := []domain.GitRefObservation{
		{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, Source: "reconciliation", Ref: "refs/heads/feature", After: ordinary.GitAfter, Provider: &identity},
		{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, Source: "reconciliation", Ref: "refs/heads/main", After: baseline, Provider: &identity},
	}
	for index := range observations {
		observations[index] = observations[index].WithID()
	}
	receipt, err := domain.NewGitHeadReceipt(claim, observations, false)
	if err != nil || receipt == nil {
		t.Fatal("invalid read fixture", err)
	}
	evidence, err := worker.core.newCodeEvidence(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	tree := strings.Repeat("f", 40)
	evidence.trees = &introductionTreeReader{commits: map[string]domain.GitCommitTree{
		root: {Commit: root, Tree: tree, Parents: []string{}}, baseline: {Commit: baseline, Tree: tree, Parents: []string{root}},
		ordinary.GitAfter: {Commit: ordinary.GitAfter, Tree: tree, Parents: []string{root}},
	}}
	prepared, err := prepareGitAdmission(ctx, frozen, *receipt, evidence)
	if err != nil {
		t.Fatal(err)
	}
	return admissionPGFixture{worker, storage, repo, frozen, prepared, claim, observations, authorization, dsn}
}

func (fixture admissionPGFixture) finish(ctx context.Context, rollback bool) (domain.GitContributionAdmission, error) {
	return repositoryWrite(evidenceWriteContext(ctx), fixture.worker.core, fixture.repo.ID, func(write context.Context) (domain.GitContributionAdmission, error) {
		current, err := fixture.worker.core.reuseGitAdmissionSnapshot(write, fixture.repo.ID, fixture.frozen.snapshot)
		if err != nil {
			return domain.GitContributionAdmission{}, err
		}
		if err := fixture.storage.FinishGitHeadScan(write, fixture.claim, fixture.observations, false, time.Now().UTC()); err != nil {
			return domain.GitContributionAdmission{}, err
		}
		admitted, err := fixture.worker.core.persistGitAdmissionInWrite(write, fixture.frozen, fixture.prepared, fixture.authorization, current)
		if err == nil && rollback {
			return domain.GitContributionAdmission{}, domain.ErrIntegrity
		}
		return admitted, err
	})
}

func TestPGGitAdmissionAtomicBaselineAndRetry(t *testing.T) {
	fixture := newAdmissionPGFixture(t)
	ctx := systemTestContext()
	before, err := fixture.storage.RepositoryRevision(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := fixture.finish(ctx, false)
	if err != nil || admitted.Validate() != nil || admitted.AfterReadVersion != fixture.claim.Version {
		t.Fatal("baseline admission failed", admitted, err)
	}
	after, err := fixture.storage.RepositoryRevision(ctx, fixture.repo.ID)
	if err != nil || after.Graph != before.Graph || after.Evidence != before.Evidence+1 {
		t.Fatalf("wrong admission revision: %+v -> %+v %v", before, after, err)
	}
	for _, expected := range []domain.Ref{fixture.frozen.source, fixture.frozen.destination} {
		got, err := fixture.storage.GetRef(ctx, fixture.repo.ID, expected.Kind, expected.Name)
		if err != nil || got != expected {
			t.Fatalf("admission changed context: %+v %v", got, err)
		}
	}
	later, err := fixture.storage.ClaimGitHeadScan(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, time.Now().UTC().Add(10*time.Minute), time.Minute)
	if err != nil || later.Version <= admitted.AfterReadVersion {
		t.Fatal("later read not claimed", later, err)
	}
	replayed, err := fixture.worker.core.persistGitAdmission(ctx, fixture.frozen, fixture.prepared, fixture.authorization)
	if err != nil || !reflect.DeepEqual(replayed, admitted) {
		t.Fatal("retry refreshed an admitted watermark", replayed, err)
	}
	replayedRevision, err := fixture.storage.RepositoryRevision(ctx, fixture.repo.ID)
	if err != nil || replayedRevision != after {
		t.Fatal("internal admission retry invalidated repository queries", replayedRevision, err)
	}
	rows, err := fixture.storage.ListGitContributionAdmissions(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 10)
	if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0], admitted) {
		t.Fatal("retry duplicated the admission", rows, err)
	}
	joins, err := fixture.storage.ListGitContributions(ctx, fixture.repo.ID)
	if err != nil || len(joins) != 0 {
		t.Fatal("baseline admission became a completed join", joins, err)
	}
}

func TestPGGitAdmissionFailureRollsBackReadAndIntent(t *testing.T) {
	for _, scenario := range []string{"outer-rollback", "revision-failure", "stale-input", "binding-revoked", "foreign-provider", "foreign-repository", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newAdmissionPGFixture(t)
			ctx := systemTestContext()
			want := domain.ErrIntegrity
			switch scenario {
			case "revision-failure":
				fixture.worker.core.meta = &githubObservationRollbackPG{PostgresStore: fixture.storage, rejectRevision: true}
			case "stale-input":
				if err := fixture.storage.AdvanceEvidenceRevision(ctx, fixture.repo.ID); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrConflict
			case "foreign-provider":
				fixture.authorization.provider.InstallationID++
				want = domain.ErrConflict
			case "foreign-repository":
				fixture.authorization.repo = hh("another admission repository")
				want = domain.ErrConflict
			case "binding-revoked":
				if err := fixture.worker.id.withIdentity(ctx, func(write context.Context) error {
					connections, err := fixture.storage.ListGitHubConnections(write)
					if err != nil {
						return err
					}
					connection := connections[0]
					connection.Enabled, connection.Generation = false, connection.Generation+1
					return fixture.storage.PutGitHubConnection(write, connection)
				}); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrForbidden
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			before, err := fixture.storage.RepositoryRevision(systemTestContext(), fixture.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			admitted, err := fixture.finish(ctx, scenario == "outer-rollback")
			if !errors.Is(err, want) || admitted.ID != "" {
				t.Fatalf("partial admission: %+v %v; want %v", admitted, err, want)
			}
			rows, err := fixture.storage.ListGitContributionAdmissions(systemTestContext(), fixture.repo.ID, fixture.repo.GitRemoteURL, "", 10)
			if err != nil || len(rows) != 0 {
				t.Fatal("admission escaped rollback", rows, err)
			}
			receipts, err := fixture.storage.ListGitHeadReceipts(systemTestContext(), fixture.repo.ID, fixture.repo.GitRemoteURL, 0, 10)
			if err != nil || len(receipts) != 0 {
				t.Fatal("read escaped rollback", receipts, err)
			}
			after, err := fixture.storage.RepositoryRevision(systemTestContext(), fixture.repo.ID)
			if err != nil || before != after {
				t.Fatal("revision escaped rollback", before, after, err)
			}
		})
	}
}
