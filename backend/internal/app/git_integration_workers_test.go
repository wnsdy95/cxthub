package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave43Capabilities struct {
	outbound.MetadataStore
	outbound.GitScanStore
	outbound.GitHeadScanStore
	outbound.GitTreeStore
	outbound.RepositoryTransactions
}

type wave43NoIOStore struct {
	wave43Capabilities
	outbound.RepositoryRevisions
	outbound.HistoryStore
}

type wave43NoIOBlobs struct{ outbound.BlobStore }
type wave43NoIORepositories struct{ outbound.RepositoryStore }

func TestWave43GitIntegrationFactoryValidationAndLazyCompatibility(test *testing.T) {
	metadata := &wave43NoIOStore{}
	valid := GitIntegrationWorkerConfig{Profile: GitIntegrationComplete, Metadata: metadata, Blobs: &wave43NoIOBlobs{}, Repositories: &wave43NoIORepositories{}, Reader: &scanReader{}, RequireCompleteMemory: true}
	var absentStore *wave43NoIOStore
	var absentBlobs *wave43NoIOBlobs
	var absentRepositories *wave43NoIORepositories
	var absentReader *scanReader
	var absentSource *wave43Source
	for _, missing := range []string{"profile", "metadata", "blobs", "repositories", "reader", "typed-metadata", "typed-blobs", "typed-repositories", "typed-reader", "typed-source", "jobs", "trees", "heads", "tree-reader", "transactions", "revision-dependencies"} {
		test.Run(missing, func(test *testing.T) {
			config := valid
			switch missing {
			case "profile":
				config.Profile = "unknown"
			case "metadata":
				config.Metadata = nil
			case "blobs":
				config.Blobs = nil
			case "repositories":
				config.Repositories = nil
			case "reader":
				config.Reader = nil
			case "typed-metadata":
				config.Metadata = absentStore
			case "typed-blobs":
				config.Blobs = absentBlobs
			case "typed-repositories":
				config.Repositories = absentRepositories
			case "typed-reader":
				config.Reader = absentReader
			case "typed-source":
				config.Authorizer = absentSource
			case "jobs":
				config.Metadata = struct {
					outbound.MetadataStore
					outbound.GitHeadScanStore
					outbound.GitTreeStore
				}{metadata, metadata, metadata}
			case "trees":
				config.Metadata = struct {
					outbound.MetadataStore
					outbound.GitScanStore
					outbound.GitHeadScanStore
				}{metadata, metadata, metadata}
			case "heads":
				config.Metadata = struct {
					outbound.MetadataStore
					outbound.GitScanStore
					outbound.GitTreeStore
				}{metadata, metadata, metadata}
			case "tree-reader":
				config.Reader = struct{ outbound.GitCommitReader }{valid.Reader}
			case "transactions":
				config.Metadata = struct {
					outbound.MetadataStore
					outbound.GitScanStore
					outbound.GitHeadScanStore
					outbound.GitTreeStore
				}{metadata, metadata, metadata, metadata}
			case "revision-dependencies":
				config.Metadata = wave43Capabilities{metadata, metadata, metadata, metadata, metadata}
			}
			if owner, err := NewGitIntegrationWorkers(config); err == nil || owner != nil {
				test.Fatal("complete constructor accepted missing capability without startup failure", owner, err)
			}
		})
	}
	if owner, err := NewGitIntegrationWorkers(valid); err != nil || owner == nil || !owner.requireCompleteMemory {
		test.Fatal("complete construction performed I/O or lost the declared memory policy", err)
	}
	valid.Profile, valid.Metadata, valid.Blobs, valid.Repositories = GitIntegrationCompatibility, wave43Capabilities{metadata, metadata, metadata, metadata, metadata}, nil, nil
	owner, err := NewGitIntegrationWorkers(valid)
	if err != nil || owner == nil || owner.revisionError == nil {
		test.Fatal("compatibility must retain the original lazy revision-query error", err)
	}
	if revision, err := owner.RepositoryRevision(context.Background(), hh(test.Name())); !errors.Is(err, owner.revisionError) || revision != (domain.RepositoryRevision{}) {
		test.Fatal("lazy revision failure was replaced or hidden", revision, err)
	}
	for _, limit := range []int{-1, 0} {
		if err := owner.Process(context.Background(), limit); err != nil {
			test.Fatal("scan nonpositive limit changed its original no-op contract", err)
		}
	}
	for _, limit := range []int{-1, 0, 9} {
		if err := owner.ProcessContributions(context.Background(), limit); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("contribution limit was not checked before capabilities or I/O", limit, err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := owner.Process(canceled, 1); !errors.Is(err, context.Canceled) {
		test.Fatal("canceled scan reached a claim", err)
	}
	if err := owner.ProcessContributions(canceled, 1); err != nil {
		test.Fatal("unsupported compatibility contribution changed its original early no-op", err)
	}
}

func wave43WorkerContext(test *testing.T, ctx context.Context, budget time.Duration) {
	test.Helper()
	_, system := inbound.RepositoryActor(ctx)
	if !system || !reflect.DeepEqual(inbound.DocumentIdentities(ctx), []domain.DocumentIdentity{domain.DocumentIdentityLegacy}) {
		test.Fatal("worker inherited caller authority or advertised unsupported document identities")
	}
	if budget > 0 {
		deadline, bounded := ctx.Deadline()
		if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > budget {
			test.Fatal("worker lost its original time budget", budget)
		}
	}
}

type wave43ScanStore struct {
	*store.FSStore
	test     *testing.T
	claims   int
	finished []domain.GitScanFinish
}

func (storage *wave43ScanStore) ClaimGitScan(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.GitScanJob, error) {
	wave43WorkerContext(storage.test, ctx, 0)
	if lease != 2*time.Minute || now.IsZero() || ctx.Value(resourcePermitKey{}) == nil {
		storage.test.Fatal("scan claim escaped its admission or original lease")
	}
	storage.claims++
	return storage.FSStore.ClaimGitScan(ctx, repo, now, lease)
}

func (storage *wave43ScanStore) FinishGitScan(ctx context.Context, finish domain.GitScanFinish) error {
	wave43WorkerContext(storage.test, ctx, 90*time.Second)
	if scope, _ := ctx.Value(revisionScopeKey{}).(string); scope != "evidence" {
		storage.test.Fatal("scan publication lost the shared evidence revision scope", scope)
	}
	storage.finished = append(storage.finished, finish)
	return storage.FSStore.FinishGitScan(ctx, finish)
}

type wave43Source struct {
	test   *testing.T
	denied error
	stale  error
	checks int
	fences int
}

func (source *wave43Source) AuthorizeGitRead(ctx context.Context, _ domain.ContentHash) (outbound.GitReadFence, error) {
	wave43WorkerContext(source.test, ctx, 90*time.Second)
	source.checks++
	if source.denied != nil {
		return nil, source.denied
	}
	return func(write context.Context) error {
		wave43WorkerContext(source.test, write, 90*time.Second)
		source.fences++
		return source.stale
	}, nil
}

func wave43ScanFixture(test *testing.T) (*wave43ScanStore, *scanReader, domain.Repo) {
	test.Helper()
	_, original := newFsckSvc(test)
	storage := &wave43ScanStore{FSStore: original, test: test}
	repo := domain.Repo{ID: hh(test.Name()), GitRemoteURL: "https://github.com/example/wave43"}
	if _, err := storage.PutRepo(systemTestContext(), repo); err != nil {
		test.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	reader := &scanReader{deltas: map[string]domain.GitCommitDelta{commit: {Commit: commit, Parents: []string{}, Changes: []domain.GitPathChange{}, Complete: true}}}
	if err := storage.EnqueueGitScan(systemTestContext(), domain.NewGitScan(repo.ID, repo.GitRemoteURL, commit, time.Now().UTC())); err != nil {
		test.Fatal(err)
	}
	return storage, reader, repo
}

func wave43CompatibilityOwner(test *testing.T, metadata outbound.MetadataStore, reader outbound.GitCommitReader, source outbound.GitSourceAuthorizer) *GitIntegrationWorkers {
	test.Helper()
	owner, err := NewGitIntegrationWorkers(GitIntegrationWorkerConfig{Profile: GitIntegrationCompatibility, Metadata: metadata, Reader: reader, Authorizer: source})
	if err != nil {
		test.Fatal(err)
	}
	return owner
}

func TestWave43GitIntegrationDirectScanPhasesRestartAndImmutableCache(test *testing.T) {
	storage, reader, repo := wave43ScanFixture(test)
	source := &wave43Source{test: test}
	ctx := inbound.WithDocumentIdentities(inbound.WithRepositoryActor(context.Background(), "untrusted-caller"), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	for phase := range 3 {
		owner := wave43CompatibilityOwner(test, storage, reader, source)
		declared := owner.workerDocumentIdentityContext(ctx)
		if actor, system := inbound.RepositoryActor(declared); actor != "untrusted-caller" || system {
			test.Fatal("support declaration itself changed the original actor")
		}
		if err := owner.Process(ctx, 1); err != nil {
			test.Fatal(err)
		}
		if len(storage.finished) != phase+1 {
			test.Fatal("direct owner did not finish exactly one phase", phase, len(storage.finished))
		}
		finish := storage.finished[phase]
		if !finish.Job.TreeIndexed || finish.Job.Indexed != (phase >= 1) || (finish.Job.State == "completed") != (phase == 2) || finish.Job.Version != int64(phase+1) {
			test.Fatal("direct owner skipped or replayed a durable scan phase", phase, finish)
		}
	}
	if reader.reads != 1 || source.checks != 3 || source.fences != 3 {
		test.Fatal("restart repeated immutable provider work", reader.reads, source.checks, source.fences)
	}
	commit := strings.Repeat("a", 40)
	cached := cachedGitEvidence{store: storage, repo: repo.ID, reader: reader}
	if delta, err := cached.ReadCommitDelta(systemTestContext(), repo.GitRemoteURL, commit, ""); err != nil || delta.Commit != commit {
		test.Fatal("existing cache helper fell back to the provider", delta, err)
	}
	if err := wave43CompatibilityOwner(test, storage, reader, source).Process(ctx, 2); err != nil || len(storage.finished) != 3 {
		test.Fatal("completed scan was claimed again", err)
	}
	if refs, err := storage.ListRefs(systemTestContext(), repo.ID); err != nil || len(refs) != 0 {
		test.Fatal("scan moved context refs", refs, err)
	}
}

func TestWave43GitIntegrationScanReadAndPublicationPolicyRemainSeparate(test *testing.T) {
	for _, boundary := range []string{"read", "publication", "offline"} {
		test.Run(boundary, func(test *testing.T) {
			storage, reader, repo := wave43ScanFixture(test)
			source := &wave43Source{test: test}
			state, reason := "retrying", "temporary_provider_or_storage_failure"
			switch boundary {
			case "read":
				source.denied = domain.ErrForbidden
			case "publication":
				source.stale = domain.ErrConflict
				state, reason = "attention", "origin_or_lease_changed"
			case "offline":
				reader.offline = true
			}
			if err := wave43CompatibilityOwner(test, storage, reader, source).Process(context.Background(), 1); err != nil {
				test.Fatal("Process must defer job failure, not change its error contract", err)
			}
			job, err := storage.GetGitScan(systemTestContext(), repo.ID, domain.NewGitScan(repo.ID, repo.GitRemoteURL, strings.Repeat("a", 40), time.Now()).ID)
			if err != nil || job.State != state || job.Reason != reason || job.TreeIndexed || job.Indexed || !job.LeaseUntil.IsZero() || !job.NextAttempt.After(job.UpdatedAt) {
				test.Fatal("denied/stale/offline scan published evidence or changed retry policy", job, err)
			}
			if source.checks != 1 || source.fences != map[string]int{"read": 0, "publication": 1, "offline": 0}[boundary] {
				test.Fatal("read authorization and publication fence changed order", source)
			}
		})
	}
}

type wave43PollingStore struct {
	*wave43ScanStore
	outbound.GitContributionWorkStore
	outbound.GitContextHeadStore
	outbound.GitContributionAdmissionStore
	repo  domain.Repo
	mutex sync.Mutex
	calls []string
	idle  bool
}

func (storage *wave43PollingStore) record(value string) {
	storage.mutex.Lock()
	defer storage.mutex.Unlock()
	storage.calls = append(storage.calls, value)
}

func (storage *wave43PollingStore) trace() []string {
	storage.mutex.Lock()
	defer storage.mutex.Unlock()
	return append([]string(nil), storage.calls...)
}

func (storage *wave43PollingStore) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return storage.repo, nil
}

func (storage *wave43PollingStore) AdvanceEvidenceRevision(context.Context, domain.ContentHash) error {
	return nil
}

func (storage *wave43PollingStore) ListRepos(ctx context.Context, _ string) ([]domain.Repo, error) {
	wave43WorkerContext(storage.test, ctx, 0)
	storage.record("repos")
	if storage.idle {
		return nil, nil
	}
	return []domain.Repo{storage.repo}, nil
}

func (storage *wave43PollingStore) ClaimGitScan(ctx context.Context, _ domain.ContentHash, _ time.Time, lease time.Duration) (domain.GitScanJob, error) {
	wave43WorkerContext(storage.test, ctx, 0)
	if lease != 2*time.Minute {
		storage.test.Fatal("scan lease changed")
	}
	storage.record("scan")
	return domain.NewGitScan(storage.repo.ID, storage.repo.GitRemoteURL, strings.Repeat("a", 40), time.Now()), nil
}

func (storage *wave43PollingStore) FinishGitScan(context.Context, domain.GitScanFinish) error {
	return nil
}

func (storage *wave43PollingStore) ClaimGitContributionWork(ctx context.Context, _ domain.ContentHash, _ time.Time, lease time.Duration) (domain.GitContributionWork, error) {
	wave43WorkerContext(storage.test, ctx, 0)
	if lease != time.Minute {
		storage.test.Fatal("contribution lease changed")
	}
	storage.record("contribution")
	return domain.GitContributionWork{RepoID: storage.repo.ID}, nil
}

func (storage *wave43PollingStore) FinishGitContributionWork(context.Context, domain.GitContributionWork) error {
	return nil
}

func TestWave43GitIntegrationRunOriginalTwoPlusTwoTickerAndReconcilerJoin(test *testing.T) {
	for _, reconcile := range []bool{false, true} {
		test.Run(map[bool]string{false: "scan-and-contributions", true: "idle-reconciler"}[reconcile], func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				base, reader, repo := wave43ScanFixture(test)
				storage := &wave43PollingStore{wave43ScanStore: base, repo: repo, idle: reconcile}
				owner := wave43CompatibilityOwner(test, storage, reader, &wave43Source{test: test, denied: domain.ErrForbidden})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				joined := make(chan struct{})
				go func() {
					defer close(joined)
					if reconcile {
						owner.RunReconciler(ctx)
					} else {
						owner.Run(ctx)
					}
				}()
				synctest.Wait()
				want, interval := []string{"scan", "scan", "repos", "contribution", "contribution"}, time.Second
				if reconcile {
					want, interval = []string{"repos"}, time.Minute
				}
				if !reflect.DeepEqual(storage.trace(), want) {
					test.Fatal("immediate original loop changed", storage.trace())
				}
				time.Sleep(interval - time.Millisecond)
				synctest.Wait()
				if !reflect.DeepEqual(storage.trace(), want) {
					test.Fatal("worker polled early", storage.trace())
				}
				time.Sleep(time.Millisecond)
				synctest.Wait()
				if !reflect.DeepEqual(storage.trace(), append(append([]string{}, want...), want...)) {
					test.Fatal("original ticker changed", storage.trace())
				}
				cancel()
				<-joined
				count := len(storage.trace())
				time.Sleep(2 * interval)
				if len(storage.trace()) != count {
					test.Fatal("worker continued after joined cancellation")
				}
			})
		})
	}
}
