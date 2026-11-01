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

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave45PRValueKey struct{}
type wave45PRTransactionKey struct{}

type wave45PRStore struct {
	*store.FSStore
	test       *testing.T
	callMu     sync.Mutex
	calls      []string
	identities []domain.DocumentIdentity
	wakeError  error
	wake       func(context.Context, domain.ContentHash, time.Time) error
	claimError error
	getRepo    func(context.Context, domain.ContentHash) (domain.Repo, error)
	finish     func(context.Context, domain.PRPromotionJob) error
	claimed    domain.PRPromotionJob
	finished   []domain.PRPromotionJob
}

func (storage *wave45PRStore) recordCall(call string) {
	storage.callMu.Lock()
	defer storage.callMu.Unlock()
	storage.calls = append(storage.calls, call)
}

func (storage *wave45PRStore) observedCalls() []string {
	storage.callMu.Lock()
	defer storage.callMu.Unlock()
	return append([]string(nil), storage.calls...)
}

func (storage *wave45PRStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	if storage.getRepo != nil {
		return storage.getRepo(ctx, repo)
	}
	return storage.FSStore.GetRepo(ctx, repo)
}

func (storage *wave45PRStore) workerContext(ctx context.Context, budget time.Duration) {
	storage.test.Helper()
	_, system := inbound.RepositoryActor(ctx)
	if !system || !reflect.DeepEqual(inbound.DocumentIdentities(ctx), storage.identities) {
		storage.test.Fatal("worker lost trusted authority or actual document support", inbound.DocumentIdentities(ctx), storage.identities)
	}
	if budget > 0 {
		deadline, bounded := ctx.Deadline()
		if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > budget {
			storage.test.Fatal("worker lost original operation deadline", budget)
		}
	}
}

func (storage *wave45PRStore) WakePRSourceJobs(ctx context.Context, repo domain.ContentHash, now time.Time) error {
	storage.workerContext(ctx, 0)
	storage.recordCall("wake")
	if repo != "" || now.IsZero() {
		storage.test.Fatal("process changed global persisted-witness reconciliation", repo, now)
	}
	if storage.wakeError != nil {
		return storage.wakeError
	}
	if storage.wake != nil {
		return storage.wake(ctx, repo, now)
	}
	return storage.FSStore.WakePRSourceJobs(ctx, repo, now)
}

func (*wave45PRStore) GetGitContributionAdmission(ctx context.Context, _ domain.ContentHash, _ string) (domain.GitContributionAdmission, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitContributionAdmission{}, err
	}
	return domain.GitContributionAdmission{}, domain.ErrNotFound
}

func (*wave45PRStore) GetGitContextHead(ctx context.Context, _ domain.ContentHash, _ string) (domain.GitContextHead, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitContextHead{}, err
	}
	return domain.GitContextHead{}, domain.ErrNotFound
}

func (*wave45PRStore) ListGitContributionProofs(ctx context.Context, _ domain.ContentHash) ([]domain.GitContributionProof, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, nil
}

func (storage *wave45PRStore) ClaimPRJob(ctx context.Context, repo domain.ContentHash, id string, now time.Time, lease time.Duration) (domain.PRPromotionJob, error) {
	storage.workerContext(ctx, 0)
	storage.recordCall("claim")
	permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
	if repo != "" || id != "" || now.IsZero() || lease != 2*time.Minute || permit == nil || !permit.live.Load() {
		storage.test.Fatal("claim lost global selection, admission, or original lease", repo, id, lease)
	}
	if storage.claimError != nil {
		return domain.PRPromotionJob{}, storage.claimError
	}
	job, err := storage.FSStore.ClaimPRJob(ctx, repo, id, now, lease)
	storage.claimed = job
	return job, err
}

func (storage *wave45PRStore) FinishPRJob(ctx context.Context, job domain.PRPromotionJob) error {
	storage.recordCall("finish:" + job.State)
	storage.finished = append(storage.finished, job)
	if storage.finish != nil {
		return storage.finish(ctx, job)
	}
	return storage.FSStore.FinishPRJob(ctx, job)
}

type wave45PRTransaction struct {
	storage *wave45PRTransactionStore
	write   bool
}

type wave45PRTransactionStore struct {
	*wave45PRStore
	fenceError    error
	fence         func(context.Context, domain.PRPromotionJob) error
	writes        int
	joins         int
	revisions     int
	appends       int
	events        []domain.HistoryEvent
	notifications []outbound.NotificationDelivery
	audits        []domain.OrganizationAuditEvent
}

func (storage *wave45PRTransactionStore) requireWrite(ctx context.Context) {
	storage.test.Helper()
	transaction, _ := ctx.Value(wave45PRTransactionKey{}).(*wave45PRTransaction)
	if transaction == nil || transaction.storage != storage || !transaction.write {
		storage.test.Fatal("promotion escaped its original shared write transaction")
	}
}

func (storage *wave45PRTransactionStore) WithinRepository(ctx context.Context, _ domain.ContentHash, write func(context.Context) error) error {
	if storage.InTransaction(ctx) {
		storage.requireWrite(ctx)
		storage.joins++
		return write(ctx)
	}
	storage.recordCall("write")
	storage.writes++
	err := write(context.WithValue(ctx, wave45PRTransactionKey{}, &wave45PRTransaction{storage: storage, write: true}))
	if err != nil {
		storage.recordCall("rollback")
		return err
	}
	storage.recordCall("commit")
	return nil
}

func (storage *wave45PRTransactionStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if storage.InTransaction(ctx) {
		storage.joins++
		return read(ctx)
	}
	return read(context.WithValue(ctx, wave45PRTransactionKey{}, &wave45PRTransaction{storage: storage}))
}

func (storage *wave45PRTransactionStore) InTransaction(ctx context.Context) bool {
	transaction, _ := ctx.Value(wave45PRTransactionKey{}).(*wave45PRTransaction)
	return transaction != nil && transaction.storage == storage
}

func (storage *wave45PRTransactionStore) InReadOnlyTransaction(ctx context.Context) bool {
	transaction, _ := ctx.Value(wave45PRTransactionKey{}).(*wave45PRTransaction)
	return transaction != nil && transaction.storage == storage && !transaction.write
}

func (storage *wave45PRTransactionStore) FencePRJob(ctx context.Context, job domain.PRPromotionJob) error {
	storage.workerContext(ctx, 90*time.Second)
	storage.requireWrite(ctx)
	storage.recordCall("fence")
	if storage.fence != nil {
		return storage.fence(ctx, job)
	}
	return storage.fenceError
}

func (storage *wave45PRTransactionStore) LockRepositoryAccess(ctx context.Context, _, _ string) error {
	storage.requireWrite(ctx)
	storage.recordCall("access")
	return nil
}

func (storage *wave45PRTransactionStore) ApplyHistoryEvent(ctx context.Context, event domain.HistoryEvent) error {
	storage.requireWrite(ctx)
	storage.recordCall("history")
	storage.events = append(storage.events, event)
	return storage.FSStore.ApplyHistoryEvent(ctx, event)
}

func (storage *wave45PRTransactionStore) AppendRef(ctx context.Context, repo domain.ContentHash, ref domain.Ref, expected domain.ContentHash) error {
	storage.requireWrite(ctx)
	storage.recordCall("append")
	storage.appends++
	return storage.FSStore.AppendRef(ctx, repo, ref, expected)
}

func (storage *wave45PRTransactionStore) AdvanceRepositoryRevision(ctx context.Context, repo domain.ContentHash, pending bool) error {
	storage.requireWrite(ctx)
	storage.recordCall("revision")
	storage.revisions++
	return storage.FSStore.AdvanceRepositoryRevision(ctx, repo, pending)
}

func (storage *wave45PRTransactionStore) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	storage.requireWrite(ctx)
	storage.audits = append(storage.audits, event)
	return storage.FSStore.AppendOrganizationAudit(ctx, event)
}

func (storage *wave45PRTransactionStore) EnqueueNotification(ctx context.Context, delivery outbound.NotificationDelivery) error {
	storage.requireWrite(ctx)
	storage.notifications = append(storage.notifications, delivery)
	return storage.FSStore.EnqueueNotification(ctx, delivery)
}

type wave45PRBlobs struct {
	*store.FSStore
	verify func(context.Context)
}

func (blobs *wave45PRBlobs) VerifyStoredDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	if blobs.verify != nil {
		blobs.verify(ctx)
	}
	return blobs.FSStore.VerifyStoredDoc(ctx, repo, hash)
}

type wave45PRFixture struct {
	config     PRPromotionWorkerConfig
	storage    *wave45PRTransactionStore
	repository domain.Repository
	outsider   domain.User
	repo       domain.ContentHash
	base       domain.ContentHash
	source     domain.ContentHash
	job        domain.PRPromotionJob
}

func newWave45PRFixture(test *testing.T, queued bool) wave45PRFixture {
	test.Helper()
	_, original := newFsckSvc(test)
	team := makeTeamFixture(test, original)
	repository := team.repository
	repository.WebhookURL = "https://example.test/wave45"
	if err := original.CreateRepository(systemTestContext(), repository); err != nil {
		test.Fatal(err)
	}
	storage := &wave45PRTransactionStore{wave45PRStore: &wave45PRStore{FSStore: original, test: test}}
	repo := hh(test.Name())
	origin := "https://github.com/example/wave45"
	ctx := systemTestContext()
	if _, err := original.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: repository.ID, DefaultBranch: "main", GitRemoteURL: origin}); err != nil {
		test.Fatal(err)
	}
	root := prSnapshot(test, original, repo, "common")
	base := prSnapshot(test, original, repo, "base", root)
	source := prSnapshot(test, original, repo, "source", root)
	if err := original.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		test.Fatal(err)
	}
	pr := domain.PullRequestMerge{Number: 45, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	now := time.Now().UTC()
	observation := domain.HistoryEvent{ID: strings.Repeat("4", 32), RepoID: string(repo), BranchID: "wave45-feature", Branch: pr.HeadBranch,
		Kind: "birth", Source: root, Target: source, GitAfter: pr.HeadSHA, CreatedAt: now.Add(-time.Minute)}
	for _, event := range []domain.HistoryEvent{observation, prPublication(observation)} {
		if err := original.ApplyHistoryEvent(ctx, event); err != nil {
			test.Fatal(err)
		}
	}
	job := domain.PRPromotionJob{ID: domain.PRPromotionID(repo, pr.Number), RepoID: repo, PR: pr,
		BaseBranchID: domain.LegacyContextBranchID(string(repo), "main"), GitOrigin: normalizeGitURL(origin), State: "waiting",
		CreatedAt: now, UpdatedAt: now, NextAttempt: now}
	if queued {
		var err error
		job, err = original.EnqueuePRJob(ctx, job)
		if err != nil {
			test.Fatal(err)
		}
	}
	blobs := &wave45PRBlobs{FSStore: original, verify: storage.requireWrite}
	config := PRPromotionWorkerConfig{Profile: PRPromotionComplete, Metadata: storage, Blobs: blobs,
		Engine: gitengine.NewEngine(storage), Repositories: storage, Evidence: storage, RequireCompleteMemory: true}
	return wave45PRFixture{config: config, storage: storage, repository: repository, outsider: team.outsider, repo: repo, base: base, source: source, job: job}
}

func wave45PRWorker(test *testing.T, fixture wave45PRFixture) *PRPromotionWorker {
	test.Helper()
	worker, err := NewPRPromotionWorker(fixture.config)
	if err != nil || worker == nil {
		test.Fatal("direct public PR factory rejected declared capabilities", err)
	}
	fixture.storage.identities = ReadDocumentIdentities(fixture.config.Metadata, fixture.config.Blobs)
	if worker.work == nil || worker.refs == nil || worker.history == nil {
		test.Fatal("factory omitted a shared policy owner")
	}
	refScope, refScoped := worker.refs.scope.(*repositoryRefScope)
	historyScope, historyScoped := worker.history.scope.(*historyCommandScope)
	historySources, historyVerified := worker.history.sources.(*documentPublicationSources)
	if !refScoped || !historyScoped || !historyVerified || refScope.work != worker.work || historyScope.work != worker.work ||
		worker.meta != fixture.config.Metadata || worker.work.meta != worker.meta || worker.refs.meta != worker.meta || worker.history.metadata != worker.meta || historySources.metadata != worker.meta ||
		worker.repositories != fixture.config.Repositories || worker.work.repositories != worker.repositories || worker.blobs != fixture.config.Blobs || worker.engine != fixture.config.Engine ||
		worker.refs.engine != worker.engine || historySources.integrity != worker.engine || worker.history.memories != worker.blobs || historySources.legacy != worker.blobs ||
		worker.requireCompleteMemory != fixture.config.RequireCompleteMemory || worker.capturedEvidence != fixture.config.CapturedEvidence {
		test.Fatal("factory split original policy/dependencies across actual owners")
	}
	return worker
}

func TestWave45PRPromotionPublicFactoryProfilesTypedNilAndNoAutostart(test *testing.T) {
	fixture := newWave45PRFixture(test, true)
	synctest.Test(test, func(test *testing.T) {
		for _, profile := range []PRPromotionProfile{PRPromotionCompatibility, PRPromotionComplete} {
			fixture.config.Profile = profile
			fixture.config.CapturedEvidence = profile == PRPromotionComplete
			wave45PRWorker(test, fixture)
			synctest.Wait()
			if len(fixture.storage.calls) != 0 {
				test.Fatal("constructor started work or adapter I/O", fixture.storage.calls)
			}
			for _, field := range []string{"Metadata", "Blobs", "Engine", "Repositories", "Evidence"} {
				invalid := fixture.config
				var missing any = (*wave45PRTransactionStore)(nil)
				if field == "Blobs" {
					missing = (*wave45PRBlobs)(nil)
				} else if field == "Engine" {
					missing = (*gitengine.Engine)(nil)
				}
				reflect.ValueOf(&invalid).Elem().FieldByName(field).Set(reflect.ValueOf(missing))
				if worker, err := NewPRPromotionWorker(invalid); worker != nil || !errors.Is(err, domain.ErrValidation) {
					test.Fatal("declared typed-nil PR dependency accepted", profile, field, err)
				}
			}
		}
		for _, profile := range []PRPromotionProfile{"", "future"} {
			invalid := fixture.config
			invalid.Profile = profile
			if worker, err := NewPRPromotionWorker(invalid); worker != nil || !errors.Is(err, domain.ErrValidation) {
				test.Fatal("unknown PR profile accepted", profile, err)
			}
		}
		for _, field := range []string{"Metadata", "Blobs", "Engine", "Repositories"} {
			invalid := fixture.config
			reflect.ValueOf(&invalid).Elem().FieldByName(field).SetZero()
			if worker, err := NewPRPromotionWorker(invalid); worker != nil || err == nil {
				test.Fatal("complete PR profile accepted absent mandatory port", field, err)
			}
		}
		if len(fixture.storage.calls) != 0 {
			test.Fatal("rejected constructor started work", fixture.storage.calls)
		}
	})
}

type wave45PRQueuePorts struct {
	outbound.MetadataStore
	outbound.PRJobStore
	outbound.RepositoryRevisions
	outbound.NotificationStore
	outbound.GitScanStore
}

type wave45PRAppendPort interface {
	AppendRef(context.Context, domain.ContentHash, domain.Ref, domain.ContentHash) error
}

type wave45PRCorePorts interface {
	outbound.MetadataStore
	outbound.PRJobStore
	outbound.RepositoryTransactions
	outbound.RepositoryTransactionPresence
	outbound.RepositoryTransactionState
	outbound.PRJobFence
	outbound.HistoryStore
	wave45PRAppendPort
}

func TestWave45PRPromotionCompleteRequiresActualCapabilities(test *testing.T) {
	fixture := newWave45PRFixture(test, false)
	storage := fixture.storage
	queue := wave45PRQueuePorts{MetadataStore: storage, PRJobStore: storage, RepositoryRevisions: storage, NotificationStore: storage, GitScanStore: storage}
	for _, missing := range []string{"jobs", "transactions-and-snapshot", "transaction-presence", "transaction-state", "fence", "history", "atomic-append", "current-doc-verifier", "repository-access", "graph-evidence", "filesystem-transactions", "repository-revisions", "notification-outbox", "history-git-scans"} {
		test.Run(missing, func(test *testing.T) {
			invalid := fixture.config
			switch missing {
			case "jobs":
				invalid.Metadata = struct {
					outbound.MetadataStore
					outbound.RepositoryTransactions
					outbound.RepositoryTransactionPresence
					outbound.RepositoryTransactionState
					outbound.PRJobFence
					outbound.HistoryStore
					wave45PRAppendPort
					outbound.RepositoryRevisions
					outbound.NotificationStore
					outbound.GitScanStore
				}{storage, storage, storage, storage, storage, storage, storage, storage, storage, storage}
			case "transactions-and-snapshot":
				invalid.Metadata = struct {
					wave45PRQueuePorts
					outbound.RepositoryTransactionPresence
					outbound.RepositoryTransactionState
					outbound.PRJobFence
					outbound.HistoryStore
					wave45PRAppendPort
				}{queue, storage, storage, storage, storage, storage}
			case "transaction-presence":
				invalid.Metadata = struct {
					wave45PRQueuePorts
					outbound.RepositoryTransactions
					outbound.RepositoryTransactionState
					outbound.PRJobFence
					outbound.HistoryStore
					wave45PRAppendPort
				}{queue, storage, storage, storage, storage, storage}
			case "transaction-state":
				invalid.Metadata = struct {
					wave45PRQueuePorts
					outbound.RepositoryTransactions
					outbound.RepositoryTransactionPresence
					outbound.PRJobFence
					outbound.HistoryStore
					wave45PRAppendPort
				}{queue, storage, storage, storage, storage, storage}
			case "fence":
				invalid.Metadata = struct {
					wave45PRQueuePorts
					outbound.RepositoryTransactions
					outbound.RepositoryTransactionPresence
					outbound.RepositoryTransactionState
					outbound.HistoryStore
					wave45PRAppendPort
				}{queue, storage, storage, storage, storage, storage}
			case "history":
				invalid.Metadata = struct {
					wave45PRQueuePorts
					outbound.RepositoryTransactions
					outbound.RepositoryTransactionPresence
					outbound.RepositoryTransactionState
					outbound.PRJobFence
					wave45PRAppendPort
				}{queue, storage, storage, storage, storage, storage}
			case "atomic-append":
				invalid.Metadata = struct {
					wave45PRQueuePorts
					outbound.RepositoryTransactions
					outbound.RepositoryTransactionPresence
					outbound.RepositoryTransactionState
					outbound.PRJobFence
					outbound.HistoryStore
				}{queue, storage, storage, storage, storage, storage}
			case "current-doc-verifier":
				invalid.Blobs = struct{ outbound.BlobStore }{storage.FSStore}
			case "repository-access":
				invalid.Repositories = struct{ outbound.RepositoryStore }{storage.FSStore}
			case "graph-evidence":
				invalid.Evidence = struct{ outbound.RepositoryReader }{storage.FSStore}
			case "filesystem-transactions":
				invalid.Metadata = storage.FSStore
			case "repository-revisions":
				invalid.Metadata = struct {
					wave45PRCorePorts
					outbound.NotificationStore
					outbound.GitScanStore
				}{storage, storage, storage}
			case "notification-outbox":
				invalid.Metadata = struct {
					wave45PRCorePorts
					outbound.RepositoryRevisions
					outbound.GitScanStore
				}{storage, storage, storage}
			case "history-git-scans":
				invalid.Metadata = struct {
					wave45PRCorePorts
					outbound.RepositoryRevisions
					outbound.NotificationStore
				}{storage, storage, storage}
			}
			if worker, err := NewPRPromotionWorker(invalid); worker != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
				test.Fatal("complete PR profile fabricated missing capability", missing, err)
			}
			if len(storage.calls) != 0 {
				test.Fatal("capability validation performed I/O", storage.calls)
			}
		})
	}
	declared := fixture.config
	declared.Metadata = struct {
		wave45PRCorePorts
		outbound.RepositoryRevisions
		outbound.NotificationStore
		outbound.GitScanStore
	}{storage, storage, storage, storage}
	if worker, err := NewPRPromotionWorker(declared); worker == nil || err != nil || len(storage.calls) != 0 {
		test.Fatal("complete rejected actual declared revision/outbox/scan ports or performed I/O", err, storage.calls)
	}
	fixture.config.Evidence = nil
	worker := wave45PRWorker(test, fixture)
	if worker.evidence != fixture.config.Metadata {
		test.Fatal("absent evidence did not default to actual metadata")
	}
}

func TestWave45PRPromotionCompatibilityAllowsAbsentOptionalPortsButRequiresQueue(test *testing.T) {
	fixture := newWave45PRFixture(test, false)
	fixture.config = PRPromotionWorkerConfig{Profile: PRPromotionCompatibility, Metadata: fixture.storage.wave45PRStore}
	worker := wave45PRWorker(test, fixture)
	if worker.evidence != fixture.config.Metadata || worker.blobs != nil || worker.engine != nil || worker.repositories != nil {
		test.Fatal("compatibility invented absent optional adapters")
	}
	if err := worker.ProcessPRPromotions(context.Background(), 1); err != nil || !reflect.DeepEqual(fixture.storage.calls, []string{"wake", "claim"}) {
		test.Fatal("empty compatibility queue eagerly used absent execution ports", err, fixture.storage.calls)
	}
	for _, metadata := range []outbound.MetadataStore{nil, struct{ outbound.MetadataStore }{fixture.storage.FSStore}} {
		config := fixture.config
		config.Metadata = metadata
		if worker, err := NewPRPromotionWorker(config); worker != nil || err == nil {
			test.Fatal("public compatibility accepted absent metadata or durable queue", err)
		}
	}
}

func TestWave45PRPromotionWakeBeforeLimitCancellationAndClaimErrorPriority(test *testing.T) {
	wakeFailure, claimFailure := errors.New("wake failed"), errors.New("claim failed")
	for _, scenario := range []struct {
		name       string
		limit      int
		canceled   bool
		wakeError  error
		claimError error
		want       error
		calls      []string
	}{
		{"negative", -1, false, nil, nil, nil, []string{"wake"}},
		{"zero", 0, false, nil, nil, nil, []string{"wake"}},
		{"empty", 1, false, nil, nil, nil, []string{"wake", "claim"}},
		{"wake-before-zero", 0, false, wakeFailure, claimFailure, wakeFailure, []string{"wake"}},
		{"wake-before-cancel", 1, true, wakeFailure, claimFailure, wakeFailure, []string{"wake"}},
		{"cancel-after-wake", 1, true, nil, claimFailure, context.Canceled, []string{"wake"}},
		{"canceled-zero", 0, true, nil, claimFailure, nil, []string{"wake"}},
		{"claim-error", 1, false, nil, claimFailure, claimFailure, []string{"wake", "claim"}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newWave45PRFixture(test, false)
			fixture.storage.wakeError, fixture.storage.claimError = scenario.wakeError, scenario.claimError
			worker := wave45PRWorker(test, fixture)
			ctx, cancel := context.WithCancel(inbound.WithDocumentIdentities(inbound.WithRepositoryActor(context.Background(), "caller"), []domain.DocumentIdentity{"future"}))
			defer cancel()
			if scenario.canceled {
				cancel()
			}
			err := worker.ProcessPRPromotions(ctx, scenario.limit)
			if !errors.Is(err, scenario.want) || !reflect.DeepEqual(fixture.storage.calls, scenario.calls) {
				test.Fatal("original wake/limit/cancel/claim priority changed", err, fixture.storage.calls)
			}
		})
	}
}

func TestWave45PRPromotionRunKeepsExplicitOneSecondLifecycle(test *testing.T) {
	fixture := newWave45PRFixture(test, false)
	fixture.storage.wake = func(context.Context, domain.ContentHash, time.Time) error { return nil }
	fixture.storage.claimError = domain.ErrNotFound
	synctest.Test(test, func(test *testing.T) {
		worker := wave45PRWorker(test, fixture)
		ctx, gate := admissionTestContext(time.Second)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		finished := make(chan struct{})
		go func() {
			worker.Run(ctx)
			close(finished)
		}()
		synctest.Wait()
		if calls := fixture.storage.observedCalls(); !reflect.DeepEqual(calls, []string{"wake", "claim"}) {
			test.Fatal("explicit Run did not process immediately", calls)
		}
		time.Sleep(time.Second - time.Millisecond)
		synctest.Wait()
		if calls := fixture.storage.observedCalls(); len(calls) != 2 {
			test.Fatal("worker polled before its original one-second tick", calls)
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if calls := fixture.storage.observedCalls(); !reflect.DeepEqual(calls, []string{"wake", "claim", "wake", "claim"}) {
			test.Fatal("worker lost its original one-second tick", calls)
		}
		cancel()
		synctest.Wait()
		select {
		case <-finished:
		default:
			test.Fatal("explicit worker did not stop on caller cancellation")
		}
		assertResourcesReleased(test, gate)
	})
}

func TestWave45PRPromotionClaimRunDetachedFinishRetainsOriginalContracts(test *testing.T) {
	fixture := newWave45PRFixture(test, true)
	worker := wave45PRWorker(test, fixture)
	ctx, gate := admissionTestContext(time.Second)
	ctx, cancel := context.WithCancel(context.WithValue(ctx, wave45PRValueKey{}, "request-value"))
	defer cancel()
	fixture.storage.fence = func(bound context.Context, job domain.PRPromotionJob) error {
		if !reflect.DeepEqual(job, fixture.storage.claimed) || job.Version == 0 || job.Attempts != 1 || job.LeaseUntil.IsZero() {
			test.Fatal("claimed lease identity changed before execution", job)
		}
		cancel()
		return bound.Err()
	}
	fixture.storage.finish = func(detached context.Context, job domain.PRPromotionJob) error {
		fixture.storage.workerContext(detached, 5*time.Second)
		if detached.Err() != nil || detached.Value(wave45PRValueKey{}) != "request-value" || fixture.storage.InTransaction(detached) ||
			job.ID != fixture.storage.claimed.ID || job.Version != fixture.storage.claimed.Version || job.Attempts != 1 ||
			job.State != "retrying" || job.Reason != "temporary_failure" || job.FailureClass != "canceled" || !job.LeaseUntil.IsZero() {
			test.Fatal("failure finish lost detached context, original fence, or retry state", job, detached.Err())
		}
		return fixture.storage.FSStore.FinishPRJob(detached, job)
	}
	if err := worker.ProcessPRPromotions(ctx, 1); err != nil {
		test.Fatal("process exposed an already-persisted run failure", err)
	}
	if !reflect.DeepEqual(fixture.storage.calls, []string{"wake", "claim", "write", "fence", "rollback", "finish:retrying"}) {
		test.Fatal("claim/run/detached-finish order changed", fixture.storage.calls)
	}
	assertResourcesReleased(test, gate)
}

func TestWave45PRPromotionFailureClassificationThresholdsAndJoinedFinishError(test *testing.T) {
	temporary := errors.New("temporary")
	for _, scenario := range []struct {
		name     string
		failure  error
		attempts int
		state    string
		reason   string
		class    string
		delay    time.Duration
	}{
		{"ordering-before-source", errors.Join(domain.ErrPROrderingPending, domain.ErrPRSourcePending), 20, "waiting", "ordering_evidence_pending", "unspecified", 256 * time.Second},
		{"source-before-integrity", errors.Join(domain.ErrPRSourcePending, domain.ErrIntegrity), 7, "waiting", "source_context_pending", "unspecified", 128 * time.Second},
		{"source-limit", domain.ErrPRSourcePending, 8, "attention", "source_finalization_required", "unspecified", 256 * time.Second},
		{"integrity", domain.ErrIntegrity, 1, "attention", "integrity_check_failed", "unspecified", 2 * time.Second},
		{"policy", domain.ErrForbidden, 1, "attention", "policy_changed", "unspecified", 2 * time.Second},
		{"validation", domain.ErrValidation, 1, "attention", "invalid_request", "unspecified", 2 * time.Second},
		{"identity", domain.ErrConflict, 1, "attention", "identity_or_history_conflict", "unspecified", 2 * time.Second},
		{"missing", domain.ErrNotFound, 1, "attention", "repository_or_base_missing", "unspecified", 2 * time.Second},
		{"ref-retry", domain.ErrRefConflict, 1, "retrying", "temporary_failure", "unspecified", 2 * time.Second},
		{"deadline", context.DeadlineExceeded, 19, "retrying", "temporary_failure", "deadline_exceeded", 256 * time.Second},
		{"retry-limit", temporary, 20, "attention", "retry_limit_reached", "unspecified", 256 * time.Second},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newWave45PRFixture(test, false)
			worker := wave45PRWorker(test, fixture)
			fixture.storage.fenceError = scenario.failure
			finishFailure := errors.New("status write failed")
			fixture.storage.finish = func(ctx context.Context, job domain.PRPromotionJob) error {
				fixture.storage.workerContext(ctx, 5*time.Second)
				if job.State != scenario.state || job.Reason != scenario.reason || job.FailureClass != scenario.class ||
					job.NextAttempt.Sub(job.UpdatedAt) != scenario.delay || !job.LeaseUntil.IsZero() {
					test.Fatal("original bounded failure/retry policy changed", job)
				}
				return finishFailure
			}
			job := fixture.job
			job.State, job.Attempts, job.Version, job.LeaseUntil = "running", scenario.attempts, 9, time.Now().Add(2*time.Minute)
			ctx, release, err := AcquireResources(worker.workerDocumentIdentityContext(systemTestContext()), ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer release()
			if _, err := worker.runPRJob(ctx, job); !errors.Is(err, scenario.failure) || !errors.Is(err, finishFailure) {
				test.Fatal("detached finish replaced rather than joined the original run error", err)
			}
		})
	}
}

func TestWave45PRPromotionPublicOwnerSharesHistoryAppendOutboxRevisionAndJobTransaction(test *testing.T) {
	fixture := newWave45PRFixture(test, true)
	worker := wave45PRWorker(test, fixture)
	fixture.storage.finish = func(ctx context.Context, job domain.PRPromotionJob) error {
		fixture.storage.requireWrite(ctx)
		fixture.storage.workerContext(ctx, 90*time.Second)
		if job.State != "completed" || job.Version != fixture.storage.claimed.Version || !job.LeaseUntil.IsZero() {
			test.Fatal("successful job completion escaped original claim identity", job)
		}
		return fixture.storage.FSStore.FinishPRJob(ctx, job)
	}
	ctx, gate := admissionTestContext(time.Second)
	if err := worker.ProcessPRPromotions(inbound.WithRepositoryActor(ctx, "untrusted-caller"), 1); err != nil {
		test.Fatal(err)
	}
	storage := fixture.storage
	if storage.writes != 1 || storage.joins < 2 || storage.revisions != 1 || storage.appends != 1 || len(storage.events) != 2 ||
		len(storage.notifications) != 1 || len(storage.audits) != 0 || len(storage.finished) != 1 || storage.calls[len(storage.calls)-1] != "commit" {
		test.Fatal("promotion split shared transaction, revision, immutable receipts, or outbox", storage.calls, storage.writes, storage.joins, storage.events)
	}
	if storage.events[0].PRCompleted || !storage.events[1].PRCompleted || storage.events[0].Source != fixture.source ||
		storage.events[0].SourceBranchID != "wave45-feature" || storage.events[1].ID != domain.PRCompletionID(storage.events[0].ID) {
		test.Fatal("promotion changed immutable source binding/completion", storage.events)
	}
	ref, err := storage.FSStore.GetRef(systemTestContext(), fixture.repo, domain.RefBranch, "main")
	job, jobError := storage.FSStore.GetPRJob(systemTestContext(), fixture.repo, fixture.job.ID)
	if err != nil || jobError != nil || ref.Target != fixture.source || job.State != "completed" || job.Attempts != 1 {
		test.Fatal("public owner did not publish actual ref and durable completion", ref, job, err, jobError)
	}
	assertResourcesReleased(test, gate)
}

func TestWave45PRPromotionCompatibilityKeepsFSPartialPublicationAndLaterRewindOnRetry(test *testing.T) {
	fixture := newWave45PRFixture(test, true)
	storage := fixture.storage.wave45PRStore
	fixture.config.Profile = PRPromotionCompatibility
	fixture.config.Metadata, fixture.config.Blobs, fixture.config.Repositories = storage, storage.FSStore, storage
	fixture.config.Engine = gitengine.NewEngine(storage)
	worker := wave45PRWorker(test, fixture)
	failed := false
	storage.finish = func(ctx context.Context, job domain.PRPromotionJob) error {
		if job.State == "completed" && !failed {
			failed = true
			return errors.New("filesystem status unavailable")
		}
		return storage.FSStore.FinishPRJob(ctx, job)
	}
	if err := worker.ProcessPRPromotions(context.Background(), 1); err != nil {
		test.Fatal(err)
	}
	ctx := worker.workerDocumentIdentityContext(systemTestContext())
	job, err := storage.FSStore.GetPRJob(ctx, fixture.repo, fixture.job.ID)
	ref, refError := storage.FSStore.GetRef(ctx, fixture.repo, domain.RefBranch, "main")
	before, historyError := storage.FSStore.ListHistoryEvents(ctx, fixture.repo)
	if !failed || err != nil || refError != nil || historyError != nil || job.State != "retrying" || ref.Target != fixture.source {
		test.Fatal("compatibility hid original partial filesystem publication", job, ref, err, refError, historyError)
	}
	ref.Target = fixture.base
	if err := storage.FSStore.CompareAndSwapRef(ctx, fixture.repo, ref, fixture.source); err != nil {
		test.Fatal(err)
	}
	if err := storage.FSStore.RetryPRJob(ctx, fixture.repo, job.ID, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	if err := worker.ProcessPRPromotions(context.Background(), 1); err != nil {
		test.Fatal(err)
	}
	after, err := storage.FSStore.ListHistoryEvents(ctx, fixture.repo)
	job, jobError := storage.FSStore.GetPRJob(ctx, fixture.repo, job.ID)
	ref, refError = storage.FSStore.GetRef(ctx, fixture.repo, domain.RefBranch, "main")
	if err != nil || jobError != nil || refError != nil || !reflect.DeepEqual(before, after) || job.State != "completed" || ref.Target != fixture.base {
		test.Fatal("retry duplicated immutable history or reapplied a completed PR over later rewind", job, ref, err, jobError, refError)
	}
}

func TestWave45PRPromotionRechecksOriginArchivedRepositoryAndDocumentIdentity(test *testing.T) {
	for _, change := range []string{"origin", "archived", "document-identity"} {
		test.Run(change, func(test *testing.T) {
			fixture := newWave45PRFixture(test, true)
			config := fixture.config
			wantReason := "policy_changed"
			repo, err := fixture.storage.FSStore.GetRepo(systemTestContext(), fixture.repo)
			if err != nil {
				test.Fatal(err)
			}
			switch change {
			case "origin":
				repo.GitRemoteURL = "https://github.com/example/replaced"
				fixture.storage.getRepo = func(context.Context, domain.ContentHash) (domain.Repo, error) { return repo, nil }
				wantReason = "identity_or_history_conflict"
			case "archived":
				repository := fixture.repository
				repository.Archived = true
				if err := fixture.storage.FSStore.CreateRepository(systemTestContext(), repository); err != nil {
					test.Fatal(err)
				}
			case "document-identity":
				if err := fixture.storage.FSStore.RequireDocumentIdentity(systemTestContext(), fixture.repo, domain.DocumentIdentityRootV1); err != nil {
					test.Fatal(err)
				}
				config.Blobs = struct {
					outbound.BlobStore
					outbound.StoredDocVerifier
				}{fixture.storage.FSStore, fixture.storage.FSStore}
			}
			fixture.config = config
			worker := wave45PRWorker(test, fixture)
			if err := worker.ProcessPRPromotions(context.Background(), 1); err != nil {
				test.Fatal(err)
			}
			job, err := fixture.storage.FSStore.GetPRJob(systemTestContext(), fixture.repo, fixture.job.ID)
			if change == "document-identity" {
				if err != nil || job.State != "waiting" || job.Attempts != 0 || len(fixture.storage.finished) != 0 {
					test.Fatal("unsupported worker claimed root-required repository", job, err)
				}
			} else if err != nil || job.State != "attention" || job.Reason != wantReason {
				test.Fatal("current origin/repository policy was not rechecked", job, err)
			}
			ref, err := fixture.storage.FSStore.GetRef(systemTestContext(), fixture.repo, domain.RefBranch, "main")
			if err != nil || ref.Target != fixture.base || fixture.storage.appends != 0 || len(fixture.storage.events) != 0 || len(fixture.storage.notifications) != 0 {
				test.Fatal("rejected worker changed ref/history/outbox", ref, err)
			}
		})
	}
}

func TestWave45PRPromotionWorkerIdentityContextPreservesActorAndPublicRefAuthority(test *testing.T) {
	fixture := newWave45PRFixture(test, false)
	worker := wave45PRWorker(test, fixture)
	for _, ctx := range []context.Context{context.Background(), inbound.WithRepositoryActor(context.Background(), "caller"), systemTestContext()} {
		actor, system := inbound.RepositoryActor(ctx)
		bound := worker.workerDocumentIdentityContext(inbound.WithDocumentIdentities(ctx, []domain.DocumentIdentity{"future"}))
		actualActor, actualSystem := inbound.RepositoryActor(bound)
		if actor != actualActor || system != actualSystem || !reflect.DeepEqual(inbound.DocumentIdentities(bound), fixture.storage.identities) {
			test.Fatal("identity declaration elevated actor or retained unsupported caller identities")
		}
	}
	input := inbound.UpdateRefInput{RepoID: fixture.repo, Ref: domain.Ref{RepoID: fixture.repo, Kind: domain.RefBranch, Name: "main", Target: fixture.base}, ExpectedTarget: fixture.base}
	for _, actor := range []string{"", fixture.outsider.ID, fixture.repository.OwnerID} {
		ctx := inbound.WithDocumentIdentities(inbound.WithRepositoryActor(context.Background(), actor), fixture.storage.identities)
		_, err := worker.UpdateRef(ctx, input)
		switch actor {
		case "":
			if !errors.Is(err, domain.ErrUnauthorized) {
				test.Fatal("anonymous actual owner mutation bypassed authority", err)
			}
		case fixture.outsider.ID:
			if !errors.Is(err, domain.ErrForbidden) {
				test.Fatal("actual owner mutation bypassed membership", err)
			}
		default:
			if err != nil {
				test.Fatal("actual owner rejected original repository owner", err)
			}
		}
	}
	if fixture.storage.revisions != 1 || len(fixture.storage.audits) != 1 || fixture.storage.audits[0].ActorID != fixture.repository.OwnerID ||
		fixture.storage.audits[0].Action != "context.ref.updated" {
		test.Fatal("actual ref owner lost original actor/audit/revision policy", fixture.storage.audits, fixture.storage.revisions)
	}
}
