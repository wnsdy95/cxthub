package app

import (
	"context"
	"errors"
	"reflect"
	"slices"
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

type wave47GitHubTransactionKey struct{}

type wave47GitHubTransaction struct {
	storage *wave47GitHubStore
	write   bool
}

type wave47GitHubStore struct {
	*store.FSStore
	writes     int
	reads      int
	identities int
}

func (storage *wave47GitHubStore) WithinRepository(ctx context.Context, _ domain.ContentHash, execute func(context.Context) error) error {
	if storage.InTransaction(ctx) {
		return execute(ctx)
	}
	storage.writes++
	return execute(context.WithValue(ctx, wave47GitHubTransactionKey{}, wave47GitHubTransaction{storage: storage, write: true}))
}

func (storage *wave47GitHubStore) WithinReadSnapshot(ctx context.Context, execute func(context.Context) error) error {
	if storage.InTransaction(ctx) {
		return execute(ctx)
	}
	storage.reads++
	return execute(context.WithValue(ctx, wave47GitHubTransactionKey{}, wave47GitHubTransaction{storage: storage}))
}

func (storage *wave47GitHubStore) InTransaction(ctx context.Context) bool {
	transaction, present := ctx.Value(wave47GitHubTransactionKey{}).(wave47GitHubTransaction)
	return present && transaction.storage == storage
}

func (storage *wave47GitHubStore) InReadOnlyTransaction(ctx context.Context) bool {
	transaction, present := ctx.Value(wave47GitHubTransactionKey{}).(wave47GitHubTransaction)
	return present && transaction.storage == storage && !transaction.write
}

func (storage *wave47GitHubStore) WithinIdentity(ctx context.Context, execute func(context.Context) error) error {
	storage.identities++
	return storage.WithinRepository(ctx, "", execute)
}

func (*wave47GitHubStore) LockRepositoryAccess(context.Context, string, string) error {
	return nil
}

func (*wave47GitHubStore) GetGitContributionAdmission(ctx context.Context, _ domain.ContentHash, _ string) (domain.GitContributionAdmission, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitContributionAdmission{}, err
	}
	return domain.GitContributionAdmission{}, domain.ErrNotFound
}

func (*wave47GitHubStore) GetGitContextHead(ctx context.Context, _ domain.ContentHash, _ string) (domain.GitContextHead, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitContextHead{}, err
	}
	return domain.GitContextHead{}, domain.ErrNotFound
}

func (*wave47GitHubStore) ListGitContributionProofs(ctx context.Context, _ domain.ContentHash) ([]domain.GitContributionProof, error) {
	return nil, ctx.Err()
}

func wave47GitHubConfig(storage *wave47GitHubStore, remote outbound.GitHubApp) GitHubWorkerConfig {
	return GitHubWorkerConfig{Profile: GitHubWorkerComplete, Store: storage, Remote: remote, Metadata: storage,
		Blobs: storage, Repositories: storage, Organizations: storage, Teams: storage, IdentityTransactions: storage,
		Evidence: storage, Memory: storage, RequireCompleteMemory: true}
}

func wave47GitHubOwner(test *testing.T, config GitHubWorkerConfig) *GitHubWorker {
	test.Helper()
	worker, err := NewGitHubWorker(config)
	if err != nil || worker == nil {
		test.Fatal("public GitHub owner rejected explicit dependencies", err)
	}
	if worker.work == nil || worker.publications == nil || worker.publications.work != worker.work ||
		worker.store != config.Store || worker.metadata != config.Metadata || worker.remote != config.Remote ||
		worker.repositories != config.Repositories || worker.organizations != config.Organizations || worker.teams != config.Teams ||
		worker.transactions != config.IdentityTransactions || worker.work.meta != config.Metadata || worker.work.repositories != config.Repositories ||
		worker.publications.requireCompleteMemory != config.RequireCompleteMemory {
		test.Fatal("public GitHub owner split its original work, policy, or publication dependencies")
	}
	return worker
}

func TestWave47GitHubPublicFactoryProfilesTypedNilAndNoIO(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		storage := &wave47GitHubStore{}
		remote := &githubFake{}
		config := wave47GitHubConfig(storage, remote)
		for _, profile := range []GitHubWorkerProfile{GitHubWorkerCompatibility, GitHubWorkerComplete} {
			config.Profile = profile
			worker := wave47GitHubOwner(test, config)
			if worker.publications.evidence != storage || worker.publications.memory != storage {
				test.Fatal("explicit evidence or engine-independent memory source replaced")
			}
			for _, field := range []string{"Store", "Metadata", "Remote", "Blobs", "Repositories", "Organizations", "Teams", "IdentityTransactions", "Evidence", "Memory"} {
				invalid := config
				var absent any = (*wave47GitHubStore)(nil)
				if field == "Remote" {
					absent = (*githubFake)(nil)
				}
				value := reflect.ValueOf(&invalid).Elem().FieldByName(field)
				value.Set(reflect.ValueOf(absent))
				if value.IsNil() || !value.Elem().IsNil() {
					test.Fatal("fixture did not declare an actual typed-nil interface", field)
				}
				if owner, err := NewGitHubWorker(invalid); owner != nil || !errors.Is(err, domain.ErrValidation) {
					test.Fatal("declared typed-nil dependency accepted", profile, field, err)
				}
			}
			for _, field := range []string{"Store", "Metadata"} {
				invalid := config
				reflect.ValueOf(&invalid).Elem().FieldByName(field).SetZero()
				if owner, err := NewGitHubWorker(invalid); owner != nil || !errors.Is(err, domain.ErrValidation) {
					test.Fatal("mandatory dependency accepted as absent", profile, field, err)
				}
			}
		}
		for _, profile := range []GitHubWorkerProfile{"", "future"} {
			invalid := config
			invalid.Profile = profile
			if owner, err := NewGitHubWorker(invalid); owner != nil || !errors.Is(err, domain.ErrValidation) {
				test.Fatal("unknown profile accepted", profile, err)
			}
		}
		config.Evidence, config.Memory = nil, nil
		worker := wave47GitHubOwner(test, config)
		memory, explicit := worker.publications.memory.(serviceProjectionSource)
		if worker.publications.evidence != storage || !explicit || memory.MemoryMetadataReader != storage || memory.MemoryObjectReader != storage {
			test.Fatal("omitted sources did not default to explicit metadata and blobs")
		}
		synctest.Wait()
		if storage.writes != 0 || storage.reads != 0 || storage.identities != 0 || remote.calls != 0 {
			test.Fatal("constructor started hidden work or adapter I/O")
		}
	})
}

func TestWave47GitHubCompleteRequiresActualCapabilities(test *testing.T) {
	storage := &wave47GitHubStore{}
	config := wave47GitHubConfig(storage, &githubFake{})
	for _, field := range []string{"Remote", "Blobs", "Repositories", "Organizations", "Teams", "IdentityTransactions"} {
		invalid := config
		reflect.ValueOf(&invalid).Elem().FieldByName(field).SetZero()
		if owner, err := NewGitHubWorker(invalid); owner != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
			test.Fatal("Complete accepted an absent operational capability", field, err)
		}
	}
	for _, missing := range []outbound.MetadataStore{
		struct {
			*wave47GitHubStore
			WithinRepository bool
		}{wave47GitHubStore: storage},
		struct {
			*wave47GitHubStore
			WithinReadSnapshot bool
		}{wave47GitHubStore: storage},
		struct {
			*wave47GitHubStore
			InTransaction bool
		}{wave47GitHubStore: storage},
		struct {
			*wave47GitHubStore
			InReadOnlyTransaction bool
		}{wave47GitHubStore: storage},
		struct {
			*wave47GitHubStore
			RepositoryRevision bool
		}{wave47GitHubStore: storage},
		struct {
			*wave47GitHubStore
			ListHistoryEvents bool
		}{wave47GitHubStore: storage},
		struct {
			*wave47GitHubStore
			EnqueuePRJob bool
		}{wave47GitHubStore: storage},
		struct {
			*wave47GitHubStore
			RecordGitRefObservation bool
		}{wave47GitHubStore: storage},
	} {
		invalid := config
		invalid.Metadata = missing
		if owner, err := NewGitHubWorker(invalid); owner != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
			test.Fatalf("Complete accepted metadata with a hidden mandatory capability: %T %v", missing, err)
		}
	}
	invalid := config
	invalid.Repositories = struct{ outbound.RepositoryStore }{storage}
	if owner, err := NewGitHubWorker(invalid); owner != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("Complete accepted repositories without the current access lock", err)
	}
	invalid = config
	invalid.Evidence = struct{ outbound.RepositoryReader }{storage}
	if owner, err := NewGitHubWorker(invalid); owner != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("Complete accepted a repository-only evidence facade", err)
	}
	for _, missing := range []outbound.MetadataStore{
		struct {
			*wave47GitHubStore
			SavePRHistoryCheckpoint bool
		}{wave47GitHubStore: storage},
		struct {
			*wave47GitHubStore
			PutPRContribution bool
		}{wave47GitHubStore: storage},
	} {
		optional := config
		optional.Metadata = missing
		wave47GitHubOwner(test, optional)
		optional.Remote = &githubHistoryRemote{GitHubApp: config.Remote}
		if owner, err := NewGitHubWorker(optional); owner != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
			test.Fatal("enabled provider history accepted missing actual history or contribution storage", err)
		}
	}
}

type wave47GitHubQueue struct {
	outbound.GitHubStore
	mutex    sync.Mutex
	contexts []context.Context
	listErr  error
}

func (storage *wave47GitHubQueue) ListGitHubConnections(ctx context.Context) ([]domain.GitHubConnection, error) {
	storage.mutex.Lock()
	defer storage.mutex.Unlock()
	storage.contexts = append(storage.contexts, ctx)
	return nil, storage.listErr
}

func (*wave47GitHubQueue) ListGitHubDeliveries(context.Context) ([]domain.GitHubDelivery, error) {
	return nil, nil
}

func (storage *wave47GitHubQueue) snapshot() []context.Context {
	storage.mutex.Lock()
	defer storage.mutex.Unlock()
	return append([]context.Context(nil), storage.contexts...)
}

func TestWave47GitHubRunImmediateTwentySecondPollNinetySecondDeadlineAndJoinedCancellation(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		queue := &wave47GitHubQueue{}
		worker := wave47GitHubOwner(test, GitHubWorkerConfig{Profile: GitHubWorkerCompatibility, Store: queue,
			Metadata: struct{ outbound.MetadataStore }{}, Remote: nil})
		ctx, cancel := context.WithCancel(inbound.WithRepositoryActor(context.Background(), "original-actor"))
		defer cancel()
		started := time.Now()
		done := make(chan struct{})
		go func() {
			defer close(done)
			worker.Run(ctx)
		}()
		synctest.Wait()
		contexts := queue.snapshot()
		if len(contexts) != 1 {
			test.Fatal("Run did not execute its immediate actual tick", len(contexts))
		}
		deadline, bounded := contexts[0].Deadline()
		actor, system := inbound.RepositoryActor(contexts[0])
		if !bounded || deadline.Sub(started) != 90*time.Second || actor != "original-actor" || system ||
			!reflect.DeepEqual(inbound.DocumentIdentities(contexts[0]), worker.work.supported) {
			test.Fatal("Run changed its ninety-second budget, caller actor, or actual document support")
		}
		time.Sleep(20*time.Second - time.Nanosecond)
		synctest.Wait()
		if len(queue.snapshot()) != 1 {
			test.Fatal("Run polled before its original twenty-second interval")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if len(queue.snapshot()) != 2 {
			test.Fatal("Run lost its original twenty-second interval")
		}
		cancel()
		<-done
		time.Sleep(40 * time.Second)
		if contexts := queue.snapshot(); len(contexts) != 2 || contexts[0].Err() != context.Canceled || contexts[1].Err() != context.Canceled {
			test.Fatal("Run did not cancel child ticks and join without further work")
		}
	})
}

type wave47GitHubRemote struct {
	*githubFake
	storage *wave47GitHubStore
	test    *testing.T
}

func (remote *wave47GitHubRemote) checkOutsideTransaction(ctx context.Context) {
	remote.test.Helper()
	if remote.storage.InTransaction(ctx) || ctx.Value(identityMutationKey{}) != nil || ctx.Value(afterCommitKey{}) != nil {
		remote.test.Fatal("GitHub provider read retained an identity or repository transaction")
	}
}

func (remote *wave47GitHubRemote) Installation(ctx context.Context, installation int64) (domain.GitHubInstallation, error) {
	remote.checkOutsideTransaction(ctx)
	return remote.githubFake.Installation(ctx, installation)
}

func (remote *wave47GitHubRemote) TeamMembers(ctx context.Context, installation int64, organization, team string) ([]int64, error) {
	remote.checkOutsideTransaction(ctx)
	return remote.githubFake.TeamMembers(ctx, installation, organization, team)
}

func wave47GitHubFixture(test *testing.T) (*GitHubWorker, *GitHubConnections, *wave47GitHubStore, teamFixture, *githubFake, domain.Repo) {
	test.Helper()
	commands, original, team, remote := githubFixture(test)
	storage := &wave47GitHubStore{FSStore: original}
	remote.repos = []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}
	worker := wave47GitHubOwner(test, wave47GitHubConfig(storage, &wave47GitHubRemote{githubFake: remote, storage: storage, test: test}))
	ctx := systemTestContext()
	state := startGitHub(test, commands, team.owner.ID, team.organization.NamespaceID)
	if _, err := commands.Complete(ctx, team.owner.ID, state, "code"); err != nil {
		test.Fatal(err)
	}
	if err := worker.reconcile(ctx, team.organization.NamespaceID); err != nil {
		test.Fatal(err)
	}
	repo := domain.Repo{ID: hh(test.Name()), RepositoryID: team.repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := original.PutRepo(ctx, repo); err != nil {
		test.Fatal(err)
	}
	connection, err := storage.GetGitHubConnection(ctx, team.organization.NamespaceID)
	if err != nil {
		test.Fatal(err)
	}
	if err := commands.Edit(ctx, team.owner.ID, connection.NamespaceID, connection.Generation, "bind",
		domain.GitHubBinding{RepositoryID: team.repository.ID, ContextRepoID: repo.ID, ExternalID: 91}, domain.GitHubTeamMapping{}); err != nil {
		test.Fatal(err)
	}
	return worker, commands, storage, team, remote, repo
}

func TestWave47GitHubPublicReconciliationRechecksCurrentAuthorityAndGeneration(test *testing.T) {
	for _, mutation := range []string{"current", "membership", "generation"} {
		test.Run(mutation, func(test *testing.T) {
			worker, commands, storage, team, remote, _ := wave47GitHubFixture(test)
			ctx := systemTestContext()
			if err := team.identity.SetTeamRepository(ctx, team.owner.ID, team.organization.ID, team.team.ID, team.repository.ID, domain.RolePuller); err != nil {
				test.Fatal(err)
			}
			if err := storage.PutGitHubIdentity(ctx, domain.GitHubIdentity{UserID: team.member.ID, ExternalID: 124}); err != nil {
				test.Fatal(err)
			}
			connection, err := storage.GetGitHubConnection(ctx, team.organization.NamespaceID)
			if err != nil {
				test.Fatal(err)
			}
			if err := commands.Edit(ctx, team.owner.ID, connection.NamespaceID, connection.Generation, "map-team", domain.GitHubBinding{},
				domain.GitHubTeamMapping{TeamID: team.team.ID, ExternalID: 99, SyncMembers: true}); err != nil {
				test.Fatal(err)
			}
			remote.members = []int64{124, 999}
			remote.onMembers = func() {
				current, err := storage.GetGitHubConnection(ctx, connection.NamespaceID)
				if err != nil || time.Until(current.NextSync) <= 0 || time.Until(current.NextSync) > 2*time.Minute {
					test.Fatal("provider read lost original connection reservation", current, err)
				}
				if mutation == "membership" {
					if err := team.identity.RemoveOrganizationMember(ctx, team.owner.ID, team.organization.ID, team.member.ID); err != nil {
						test.Fatal(err)
					}
				} else if mutation == "generation" {
					current.Generation++
					if err := storage.PutGitHubConnection(ctx, current); err != nil {
						test.Fatal(err)
					}
				}
			}
			err = worker.reconcile(ctx, connection.NamespaceID)
			if mutation == "generation" && !errors.Is(err, domain.ErrConflict) || mutation != "generation" && err != nil {
				test.Fatal("current generation or membership fence changed", mutation, err)
			}
			role, granted := team.identity.RoleOf(ctx, team.repository.ID, team.member.ID)
			if mutation == "current" {
				if !granted || role != domain.RolePuller {
					test.Fatal("actual owner did not publish a currently authorized GitHub team grant", role, granted)
				}
				current, err := storage.GetGitHubConnection(ctx, connection.NamespaceID)
				if err != nil || current.Status != "connected" || current.UnresolvedMembers != 1 || time.Until(current.NextSync) <= 0 || time.Until(current.NextSync) > 5*time.Minute {
					test.Fatal("actual reconciliation changed successful authority refresh or unresolved identity accounting", current, err)
				}
			} else if granted {
				test.Fatal("provider result revived authority revoked during external I/O")
			}
		})
	}
}

func TestWave47GitHubPublicEnqueuePreservesRevisionAndRejectsStaleBinding(test *testing.T) {
	worker, _, storage, team, _, repo := wave47GitHubFixture(test)
	ctx := systemTestContext()
	connection, err := storage.GetGitHubConnection(ctx, team.organization.NamespaceID)
	if err != nil || len(connection.Bindings) != 1 {
		test.Fatal("missing explicit fixture binding", err)
	}
	before, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil {
		test.Fatal(err)
	}
	request := domain.PullRequestMerge{Number: 47, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	for range 2 {
		if err := worker.enqueuePR(ctx, connection, connection.Bindings[0], request); err != nil {
			test.Fatal(err)
		}
	}
	after, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil || after != before {
		test.Fatal("acceptance without publication invalidated a repository revision", before, after, err)
	}
	job, err := storage.GetPRJob(ctx, repo.ID, domain.PRPromotionID(repo.ID, request.Number))
	if err != nil || job.PR != request || job.State != "waiting" {
		test.Fatal("public owner did not durably accept original PR work", job, err)
	}
	scans, err := storage.ListGitScans(ctx, repo.ID, "", 100)
	if err != nil || len(scans) != 2 {
		test.Fatal("duplicate PR acceptance lost endpoint scan idempotency", scans, err)
	}
	connection.Generation++
	if err := worker.enqueuePR(ctx, connection, connection.Bindings[0], request); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("stale binding queued PR work", err)
	}
}

func TestWave47GitHubPublicDeliveryPublishesEvidenceAndPRWithoutMovingContext(test *testing.T) {
	worker, commands, storage, _, _, repo := wave47GitHubFixture(test)
	ctx := systemTestContext()
	body := githubPRObservationBody("closed", strings.Repeat("a", 40), strings.Repeat("b", 40))
	before, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil {
		test.Fatal(err)
	}
	if err := commands.Receive(ctx, "wave47-closed", "pull_request", body); err != nil {
		test.Fatal(err)
	}
	worker = wave47GitHubOwner(test, wave47GitHubConfig(storage, worker.remote))
	if err := worker.Tick(ctx); err != nil {
		test.Fatal(err)
	}
	delivery, err := storage.GetGitHubDelivery(ctx, githubHash("wave47-closed"))
	if err != nil || !delivery.Done || len(delivery.Body) != 0 || delivery.BodyHash != githubHash(string(body)) ||
		delivery.Attempts != 1 || !delivery.LeaseUntil.IsZero() {
		test.Fatal("public Tick did not complete actual delivery with retained idempotency proof", delivery, err)
	}
	job, err := storage.GetPRJob(ctx, repo.ID, domain.PRPromotionID(repo.ID, 7))
	if err != nil || job.State != "waiting" || job.PR.HeadSHA != strings.Repeat("b", 40) || job.PR.MergeSHA != strings.Repeat("c", 40) {
		test.Fatal("actual delivery did not accept merged PR", job, err)
	}
	observations, err := storage.ListPRRefObservations(ctx, repo.ID, repo.GitRemoteURL, 91, 7, "", 100)
	if err != nil || len(observations) != 1 || observations[0].Validate() != nil || observations[0].PullRequest == nil ||
		observations[0].PullRequest.ID != 701 || observations[0].Delivery != delivery.ID {
		test.Fatal("delivery lost durable provider identity evidence", observations, err)
	}
	after, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil || after.Graph != before.Graph || after.Evidence <= before.Evidence {
		test.Fatal("webhook publication changed graph rather than only evidence", before, after, err)
	}
	history, err := storage.ListHistoryEvents(ctx, repo.ID)
	if err != nil || len(history) != 0 {
		test.Fatal("webhook evidence manufactured context history", history, err)
	}
	refs, err := storage.ListRefs(ctx, repo.ID)
	if err != nil || len(refs) != 0 {
		test.Fatal("webhook evidence moved current context", refs, err)
	}
	if err := worker.deliver(ctx, delivery.ID); err != nil {
		test.Fatal(err)
	}
	replayed, err := storage.GetGitHubDelivery(ctx, delivery.ID)
	if err != nil || !reflect.DeepEqual(replayed, delivery) {
		test.Fatal("completed delivery replay changed its lease or attempts", replayed, err)
	}
}

type wave47GitHubLeaseStore struct {
	outbound.GitHubStore
	test  *testing.T
	reads int
}

func (storage *wave47GitHubLeaseStore) GetGitHubDelivery(ctx context.Context, identifier string) (domain.GitHubDelivery, error) {
	job, err := storage.GitHubStore.GetGitHubDelivery(ctx, identifier)
	storage.reads++
	if err == nil && storage.reads == 2 {
		job.Lease = "wave47-replacement-lease"
		job.LeaseUntil = time.Now().UTC().Add(2 * time.Minute)
		err = storage.GitHubStore.PutGitHubDelivery(ctx, job)
	}
	return job, err
}

func (storage *wave47GitHubLeaseStore) PutGitHubDelivery(ctx context.Context, job domain.GitHubDelivery) error {
	storage.test.Helper()
	if job.Lease == "" || job.Attempts != 1 || time.Until(job.LeaseUntil) <= 0 || time.Until(job.LeaseUntil) > 2*time.Minute {
		storage.test.Fatal("actual delivery claim changed its original two-minute lease or first attempt", job)
	}
	return storage.GitHubStore.PutGitHubDelivery(ctx, job)
}

func TestWave47GitHubPublicDeliveryLeaseFenceAndRestartRetainIdempotentEvidence(test *testing.T) {
	_, commands, storage, _, remote, repo := wave47GitHubFixture(test)
	ctx := systemTestContext()
	body := githubPRObservationBody("closed", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err := commands.Receive(ctx, "wave47-lease", "pull_request", body); err != nil {
		test.Fatal(err)
	}
	config := wave47GitHubConfig(storage, remote)
	config.Store = &wave47GitHubLeaseStore{GitHubStore: storage, test: test}
	worker := wave47GitHubOwner(test, config)
	identifier := githubHash("wave47-lease")
	if err := worker.deliver(ctx, identifier); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("stale delivery owner acknowledged a successor lease", err)
	}
	pending, err := storage.GetGitHubDelivery(ctx, identifier)
	if err != nil || pending.Done || pending.Lease != "wave47-replacement-lease" || string(pending.Body) != string(body) {
		test.Fatal("stale delivery completion removed recoverable bytes or successor lease", pending, err)
	}
	pending.LeaseUntil, pending.NextAttempt = time.Now().UTC().Add(-time.Second), time.Now().UTC().Add(-time.Second)
	if err := storage.PutGitHubDelivery(ctx, pending); err != nil {
		test.Fatal(err)
	}
	worker = wave47GitHubOwner(test, wave47GitHubConfig(storage, remote))
	if err := worker.deliver(ctx, identifier); err != nil {
		test.Fatal(err)
	}
	finished, err := storage.GetGitHubDelivery(ctx, identifier)
	if err != nil || !finished.Done || len(finished.Body) != 0 || finished.Attempts != 2 {
		test.Fatal("restarted actual owner did not recover original delivery", finished, err)
	}
	observations, err := storage.ListPRRefObservations(ctx, repo.ID, repo.GitRemoteURL, 91, 7, "", 100)
	if err != nil || len(observations) != 1 {
		test.Fatal("lease recovery duplicated durable PR evidence", observations, err)
	}
	scans, err := storage.ListGitScans(ctx, repo.ID, "", 100)
	if err != nil || len(scans) != 2 {
		test.Fatal("lease recovery duplicated or lost endpoint scans", scans, err)
	}
}

func TestWave47GitHubPublicDeliveryIdentityAndIntegrityFencesRetainBytes(test *testing.T) {
	for _, mutation := range []string{"body", "document", "source"} {
		test.Run(mutation, func(test *testing.T) {
			worker, commands, storage, _, _, repo := wave47GitHubFixture(test)
			ctx := systemTestContext()
			body := githubPRObservationBody("synchronize", strings.Repeat("a", 40), strings.Repeat("b", 40))
			if err := commands.Receive(ctx, "wave47-fenced", "pull_request", body); err != nil {
				test.Fatal(err)
			}
			identifier := githubHash("wave47-fenced")
			before, err := storage.GetGitHubDelivery(ctx, identifier)
			if err != nil {
				test.Fatal(err)
			}
			if mutation == "body" {
				before.Body = append(append([]byte(nil), before.Body...), ' ')
				if err := storage.PutGitHubDelivery(ctx, before); err != nil {
					test.Fatal(err)
				}
			} else if mutation == "document" {
				config := wave47GitHubConfig(storage, worker.remote)
				config.Profile, config.Blobs = GitHubWorkerCompatibility, nil
				worker = wave47GitHubOwner(test, config)
				if err := storage.RequireDocumentIdentity(ctx, repo.ID, domain.DocumentIdentityRootV1); err != nil {
					test.Fatal(err)
				}
			} else {
				connections, err := storage.ListGitHubConnections(ctx)
				if err != nil || len(connections) != 1 || len(connections[0].Bindings) != 1 {
					test.Fatal("missing source binding fixture", connections, err)
				}
				connection := connections[0]
				connection.Bindings[0].GitOrigin = "https://github.com/other/repository.git"
				if err := storage.PutGitHubConnection(ctx, connection); err != nil {
					test.Fatal(err)
				}
			}
			err = worker.deliver(ctx, identifier)
			if mutation == "body" && !errors.Is(err, domain.ErrIntegrity) || mutation == "document" && !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || mutation == "source" && err != nil {
				test.Fatal("delivery changed pre-claim integrity, document, or retry error behavior", mutation, err)
			}
			after, err := storage.GetGitHubDelivery(ctx, identifier)
			if err != nil || after.Done || string(after.Body) != string(before.Body) {
				test.Fatal("failed source validation consumed durable delivery bytes", after, err)
			}
			if mutation != "source" && !reflect.DeepEqual(after, before) {
				test.Fatal("pre-claim policy failure spent an attempt or changed a lease", before, after)
			}
			if mutation == "source" && (after.Attempts != 1 || !after.LeaseUntil.IsZero() || time.Until(after.NextAttempt) <= 0 || time.Until(after.NextAttempt) > time.Minute) {
				test.Fatal("failed delivery lost original one-minute retry and released lease", after)
			}
			scans, err := storage.ListGitScans(ctx, repo.ID, "", 100)
			if err != nil || len(scans) != 0 {
				test.Fatal("fenced delivery published scans", scans, err)
			}
		})
	}
}

func TestWave47GitHubPublicHistoryRestartPublishesPinnedContextAndMemoryWithoutMovingMain(test *testing.T) {
	fixture := newPRMemoryFixture(test)
	ctx := systemTestContext()
	selected := fixture.scanMemoryCode(test)
	first := fixture.digest(test, "", "removed claim", "retained claim")
	latest := fixture.digest(test, first, "retained claim")
	mutable := fixture.digest(test, latest, "MUTABLE POINTER MUST NOT WIN")
	if err := fixture.storage.CompareAndSwapSnapshotMemory(ctx, fixture.repo.ID, fixture.ordinary.Target, "", mutable); err != nil {
		test.Fatal(err)
	}
	publication := fixture.publish(test, first, true)
	storage := &wave47GitHubStore{FSStore: fixture.storage}
	config := wave47GitHubConfig(storage, fixture.remote)
	worker := wave47GitHubOwner(test, config)
	historyBefore, err := fixture.storage.ListHistoryEvents(ctx, fixture.repo.ID)
	if err != nil {
		test.Fatal(err)
	}
	work := historyWork(test, fixture.worker, fixture.storage, fixture.repo)
	if err := worker.reconcilePRHistory(ctx, fixture.remote, storage, work); err != nil {
		test.Fatal(err)
	}
	checkpoint, err := storage.GetPRHistoryCheckpoint(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, 91, 7)
	if err != nil || checkpoint.Version != 2 || checkpoint.Cursor != "page-2" || checkpoint.Complete {
		test.Fatal("actual owner did not persist its first history page", checkpoint, err)
	}
	worker = wave47GitHubOwner(test, config)
	if err := worker.reconcilePRHistory(ctx, fixture.remote, storage, historyWork(test, fixture.worker, storage, fixture.repo)); err != nil {
		test.Fatal(err)
	}
	contributions, err := storage.ListPRContributions(ctx, fixture.repo.ID)
	if err != nil || len(contributions) != 1 || contributions[0].Capture != fixture.ordinary.Target || contributions[0].OriginalHead != fixture.ordinary.GitAfter {
		test.Fatal("public owner did not publish genuine original capture contribution", contributions, err)
	}
	memories, err := storage.ListPRContributionMemories(ctx, fixture.repo.ID)
	if err != nil || len(memories) != 1 || memories[0].PublicationID != publication.ID || memories[0].MemoryHash != first || !memories[0].MemoryPinned {
		test.Fatal("actual publication command lost original immutable memory pin", memories, err)
	}
	publication = fixture.publish(test, latest, true)
	if err := worker.reconcilePRHistory(ctx, fixture.remote, storage, historyWork(test, fixture.worker, storage, fixture.repo)); err != nil {
		test.Fatal(err)
	}
	page, err := fixture.worker.core.QueryEffectiveMemory(ctx, fixture.repo.ID, domain.EffectiveMemoryRequest{
		Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: fixture.main.Target, CodeCommit: selected}, Content: "claims"})
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Text != "retained claim" || page.Items[0].State != "applied" ||
		!slices.Contains(page.Items[0].PublicationIDs, publication.ID) || page.Items[0].IntegrationReceipt == "" {
		test.Fatal("actual owner publication did not reach context-aware effective memory", page, err)
	}
	query, err := fixture.worker.core.QueryContext(ctx, fixture.repo.ID, domain.ContextSelection{Scope: "current", Branch: "main", Position: "main"})
	if err != nil || query.Inclusion == nil || !slices.Contains(query.Inclusion.SnapshotIDs, fixture.ordinary.Target) {
		test.Fatal("actual owner publication did not reach current context selection", query.Inclusion, err)
	}
	main, err := storage.GetRef(ctx, fixture.repo.ID, domain.RefBranch, "main")
	if err != nil || main != fixture.main {
		test.Fatal("history reconciliation rewound or replaced latest main work", main, err)
	}
	historyAfter, err := storage.ListHistoryEvents(ctx, fixture.repo.ID)
	if err != nil || len(historyAfter) != len(historyBefore)+1 || len(fixture.remote.calls) != 2 {
		test.Fatal("supplement manufactured history or reread complete provider history", historyAfter, fixture.remote.calls, err)
	}
	for _, original := range historyBefore {
		if !slices.ContainsFunc(historyAfter, func(current domain.HistoryEvent) bool { return reflect.DeepEqual(original, current) }) {
			test.Fatal("history reconciliation rewrote an original capture or publication", original)
		}
	}
	job, err := storage.GetPRJob(ctx, fixture.repo.ID, domain.PRPromotionID(fixture.repo.ID, 7))
	if err != nil || job.State != "completed" {
		test.Fatal("actual history contribution did not complete original PR job", job, err)
	}
}

func TestWave47GitHubPublicHistoryRevocationFencesProviderResult(test *testing.T) {
	commands, original, remote, repo, ordinary := githubHistoryFixture(test)
	ctx := systemTestContext()
	publishPRSource(test, commands.core, ordinary)
	storage := &wave47GitHubStore{FSStore: original}
	worker := wave47GitHubOwner(test, wave47GitHubConfig(storage, remote))
	work := historyWork(test, commands, storage, repo)
	remote.during = func() {
		connection := work.connection
		connection.Enabled = false
		connection.Generation++
		if err := storage.PutGitHubConnection(ctx, connection); err != nil {
			test.Fatal(err)
		}
	}
	if err := worker.reconcilePRHistory(ctx, remote, storage, work); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("revoked provider read published history evidence", err)
	}
	checkpoint, err := storage.GetPRHistoryCheckpoint(ctx, repo.ID, repo.GitRemoteURL, 91, 7)
	if err != nil || checkpoint.Version != 1 || checkpoint.Cursor != "" || checkpoint.PullRequestID != 0 || checkpoint.Complete {
		test.Fatal("revoked history advanced beyond its original reservation", checkpoint, err)
	}
	participations, err := storage.ListPRParticipations(ctx, repo.ID, repo.GitRemoteURL, 7)
	if err != nil || len(participations) != 0 {
		test.Fatal("revoked provider result published participation", participations, err)
	}
	contributions, err := storage.ListPRContributions(ctx, repo.ID)
	if err != nil || len(contributions) != 0 {
		test.Fatal("revoked provider result published context contribution", contributions, err)
	}
}

type wave47GitHubCapturedMemory struct {
	outbound.MemoryQueryReader
	reads int
}

func (reader *wave47GitHubCapturedMemory) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	reader.reads++
	return reader.MemoryQueryReader.GetMemory(ctx, repo, hash)
}

func TestWave47GitHubLegacyAssemblyRetainsCapturedSourcesAndLazyRevisionError(test *testing.T) {
	fixture := newPRMemoryFixture(test)
	service := fixture.worker.core
	storage := &wave47GitHubStore{FSStore: fixture.storage}
	hash := fixture.digest(test, "", "captured immutable memory")
	publication := fixture.publish(test, hash, true)
	memory := &wave47GitHubCapturedMemory{MemoryQueryReader: fixture.storage}
	service.projectionEvidence = storage
	service.projectionMemory = memory
	service.requireCompleteMemory = true
	service.engine, service.blobs = nil, nil
	config := fixture.worker.GitHubWorkerConfig(GitHubWorkerCompatibility)
	if config.Evidence != storage || config.Memory != service.projectionMemory || !config.RequireCompleteMemory {
		test.Fatal("legacy configuration replaced captured sources or depended on the Git engine")
	}
	worker := fixture.worker.worker()
	if worker.publications.evidence != storage || worker.publications.memory != service.projectionMemory || !worker.publications.requireCompleteMemory {
		test.Fatal("legacy owner assembly dropped captured evidence or engine-independent memory")
	}
	if source, err := worker.publications.contributionPublicationMemorySource(systemTestContext(), fixture.repo.ID, fixture.ordinary.Target, publication); err != nil || source != fixture.ordinary.Target || memory.reads != 1 {
		test.Fatal("actual publication memory command did not use captured reader", source, err)
	}
	if evidence, err := worker.publications.newCodeEvidence(systemTestContext(), fixture.repo.ID); err != nil || evidence == nil || evidence.trees != storage || evidence.deltas != storage || !evidence.requireComplete {
		test.Fatal("actual publication evidence command replaced captured reader or complete-memory policy", err)
	}
	service.meta = struct {
		*wave47GitHubStore
		RepositoryRevision bool
	}{wave47GitHubStore: storage}
	_, expected := service.RepositoryStateQueries()
	worker = fixture.worker.worker()
	if expected == nil || !errors.Is(worker.publications.revisionError, expected) {
		test.Fatal("legacy assembly eagerly rejected or erased original lazy revision-query failure")
	}
	if _, err := worker.publications.RepositoryRevision(systemTestContext(), fixture.repo.ID); !errors.Is(err, worker.publications.revisionError) {
		test.Fatal("publication replaced original lazy revision failure", err)
	}
}
