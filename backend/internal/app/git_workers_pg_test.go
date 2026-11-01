//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/gitchangeworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave42PGGitPGFixture struct {
	postgres *store.PostgresStore
	observer *pgx.Conn
	repo     domain.Repo
	team     teamFixture
	source   domain.GitHubConnection
	request  domain.GitChangeRequest
	deltas   map[string]domain.GitCommitDelta
}

func wave42PGGitPG(test *testing.T) *wave42PGGitPGFixture {
	test.Helper()
	wave40IsolatedPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 90*time.Second)
	defer cancel()
	postgres, err := store.NewPostgresStore(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(postgres.Close)
	rootCurrentSchema := "../../../schemas/db/migrations"
	if _, err := postgres.ApplyMigrations(ctx, rootCurrentSchema); err != nil {
		test.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal("invalid owned observer configuration", err)
	}
	observer, err := pgx.ConnectConfig(ctx, config.ConnConfig.Copy())
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, release := context.WithTimeout(context.Background(), 10*time.Second)
		defer release()
		if err := observer.Close(cleanup); err != nil {
			test.Error(err)
		}
	})
	team := makeTeamFixture(test, postgres)
	repo, err := postgres.PutRepo(ctx, domain.Repo{
		ID: hh(test.Name()), RepositoryID: team.repository.ID, DefaultBranch: "main",
		GitRemoteURL: "https://github.com/acme/api.git",
	})
	if err != nil {
		test.Fatal(err)
	}
	source := domain.GitHubConnection{
		NamespaceID: team.organization.NamespaceID, Enabled: true, Generation: 1,
		Status: "connected", CheckedAt: time.Now().UTC(),
		Installation: domain.GitHubInstallation{ID: 789, AccountID: 456, Kind: "Organization", Login: "acme"},
		Repositories: []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}},
		Bindings:     []domain.GitHubBinding{{RepositoryID: team.repository.ID, ContextRepoID: repo.ID, ExternalID: 91, GitOrigin: repo.GitRemoteURL}},
	}
	if err := postgres.WithinIdentity(ctx, func(bound context.Context) error {
		return postgres.PutGitHubConnection(bound, source)
	}); err != nil {
		test.Fatal(err)
	}
	request := domain.GitChangeRequest{
		Target: strings.Repeat("a", 40), TargetParent: strings.Repeat("c", 40),
		Commit: strings.Repeat("b", 40), Parent: strings.Repeat("d", 40),
	}
	change := domain.GitPathChange{Path: "feature", Before: domain.GitEntry{OID: strings.Repeat("1", 40), Mode: "100644"}, After: domain.GitEntry{OID: strings.Repeat("2", 40), Mode: "100644"}}
	deltas := map[string]domain.GitCommitDelta{
		request.Target: {Commit: request.Target, Parents: []string{request.TargetParent}, Parent: request.TargetParent, Complete: true, Changes: []domain.GitPathChange{change}},
		request.Commit: {Commit: request.Commit, Parents: []string{request.Parent}, Parent: request.Parent, Complete: true, Changes: []domain.GitPathChange{{Path: change.Path, Before: change.After, After: change.Before}}},
	}
	for _, delta := range deltas {
		if err := delta.Validate(); err != nil {
			test.Fatal("invalid original-signature Git fixture", err)
		}
	}
	return &wave42PGGitPGFixture{postgres: postgres, observer: observer, repo: repo, team: team, source: source, request: request, deltas: deltas}
}

func (fixture *wave42PGGitPGFixture) enqueue(test *testing.T) domain.GitChangeJob {
	test.Helper()
	now := time.Now().UTC()
	job := domain.GitChangeJob{
		RepoID: fixture.repo.ID, GitOrigin: fixture.repo.GitRemoteURL, Request: fixture.request,
		State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now,
	}
	job.ID = domain.GitChangeID(job.RepoID, job.GitOrigin, job.Request)
	queued, err := fixture.postgres.EnqueueGitChange(systemTestContext(), job)
	if err != nil {
		test.Fatal(err)
	}
	return queued
}

func (fixture *wave42PGGitPGFixture) job(test *testing.T, id string) domain.GitChangeJob {
	test.Helper()
	job, err := fixture.postgres.GetGitChange(systemTestContext(), fixture.repo.ID, id)
	if err != nil {
		test.Fatal(err)
	}
	var payload []byte
	var state string
	var version int64
	var nextAttempt, leaseUntil time.Time
	err = fixture.observer.QueryRow(context.Background(), `SELECT payload,state,version,next_attempt,lease_until FROM git_change_jobs WHERE repo_id=$1 AND id=$2`, fixture.repo.ID, id).Scan(&payload, &state, &version, &nextAttempt, &leaseUntil)
	var observed domain.GitChangeJob
	if err != nil {
		test.Fatal(err)
	}
	if err := json.Unmarshal(payload, &observed); err != nil || !reflect.DeepEqual(job, observed) || state != job.State || version != job.Version || !nextAttempt.Equal(job.NextAttempt.Truncate(time.Microsecond)) || !leaseUntil.Equal(job.LeaseUntil.Truncate(time.Microsecond)) {
		test.Fatal("payload and SQL scheduling columns diverged", job, observed, err)
	}
	return job
}

type wave42PGGitWorkerPGStore struct {
	*store.PostgresStore
	claims     atomic.Int64
	finishes   atomic.Int64
	reads      atomic.Int64
	cacheReads atomic.Int64
	mutex      sync.Mutex
	leases     []domain.GitChangeJob
}

func (storage *wave42PGGitWorkerPGStore) ClaimGitChange(ctx context.Context, repo domain.ContentHash, id string, now time.Time, lease time.Duration) (domain.GitChangeJob, error) {
	storage.claims.Add(1)
	if _, system := inbound.RepositoryActor(ctx); !system || storage.InTransaction(ctx) || repo != "" || id != "" || now.IsZero() || lease != 2*time.Minute || !reflect.DeepEqual(inbound.DocumentIdentities(ctx), ReadDocumentIdentities(storage, storage.PostgresStore)) {
		return domain.GitChangeJob{}, domain.ErrIntegrity
	}
	job, err := storage.PostgresStore.ClaimGitChange(ctx, repo, id, now, lease)
	if err == nil {
		storage.mutex.Lock()
		storage.leases = append(storage.leases, job)
		storage.mutex.Unlock()
	}
	return job, err
}

func (storage *wave42PGGitWorkerPGStore) FinishGitChange(ctx context.Context, job domain.GitChangeJob) error {
	storage.finishes.Add(1)
	if !storage.InTransaction(ctx) || storage.InReadOnlyTransaction(ctx) || ctx.Value(revisionScopeKey{}) != "evidence" || ctx.Value(afterCommitKey{}) == nil || !job.LeaseUntil.IsZero() {
		return domain.ErrIntegrity
	}
	return storage.PostgresStore.FinishGitChange(ctx, job)
}

func (storage *wave42PGGitWorkerPGStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	storage.reads.Add(1)
	return storage.PostgresStore.WithinReadSnapshot(ctx, read)
}

func (storage *wave42PGGitWorkerPGStore) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, commit, parent string) (domain.GitCommitDelta, error) {
	storage.cacheReads.Add(1)
	if storage.InTransaction(ctx) {
		return domain.GitCommitDelta{}, domain.ErrIntegrity
	}
	return storage.PostgresStore.GetGitDelta(ctx, repo, origin, commit, parent)
}

type wave42PGGitReader struct {
	fixture   *wave42PGGitPGFixture
	reads     atomic.Int64
	ancestry  atomic.Int64
	failure   error
	afterRead func(context.Context) error
	entered   chan struct{}
	release   <-chan struct{}
}

func (reader *wave42PGGitReader) check(ctx context.Context, origin string) error {
	deadline, bounded := ctx.Deadline()
	if outbound.GitRepository(ctx) != reader.fixture.repo.ID || origin != reader.fixture.repo.GitRemoteURL || reader.fixture.postgres.InTransaction(ctx) || !bounded || time.Until(deadline) > 90*time.Second {
		return domain.ErrIntegrity
	}
	return ctx.Err()
}

func (reader *wave42PGGitReader) ReadCommitDelta(ctx context.Context, origin, commit, parent string) (domain.GitCommitDelta, error) {
	reader.reads.Add(1)
	if err := reader.check(ctx, origin); err != nil {
		return domain.GitCommitDelta{}, err
	}
	if reader.entered != nil {
		select {
		case reader.entered <- struct{}{}:
		default:
		}
		select {
		case <-reader.release:
		case <-ctx.Done():
			return domain.GitCommitDelta{}, ctx.Err()
		}
	}
	if reader.failure != nil {
		return domain.GitCommitDelta{}, reader.failure
	}
	delta, found := reader.fixture.deltas[commit]
	if !found || parent != delta.Parent {
		return domain.GitCommitDelta{}, domain.ErrIntegrity
	}
	if commit == reader.fixture.request.Commit && reader.afterRead != nil {
		if err := reader.afterRead(ctx); err != nil {
			return domain.GitCommitDelta{}, err
		}
	}
	return delta, nil
}

func (reader *wave42PGGitReader) IsGitAncestor(ctx context.Context, origin, ancestor, descendant string) (bool, error) {
	reader.ancestry.Add(1)
	if err := reader.check(ctx, origin); err != nil {
		return false, err
	}
	if ancestor != reader.fixture.request.Target || descendant != reader.fixture.request.Parent {
		return false, domain.ErrIntegrity
	}
	return true, reader.failure
}

type wave42PGGitGrantPG struct {
	fixture *wave42PGGitPGFixture
	checks  atomic.Int64
	fences  atomic.Int64
}

func (grant *wave42PGGitGrantPG) AuthorizeGitRead(ctx context.Context, repo domain.ContentHash) (outbound.GitReadFence, error) {
	grant.checks.Add(1)
	if repo != grant.fixture.repo.ID || grant.fixture.postgres.InTransaction(ctx) {
		return nil, domain.ErrIntegrity
	}
	captured, err := grant.fixture.postgres.GetGitHubConnection(ctx, grant.fixture.source.NamespaceID)
	if err != nil {
		return nil, err
	}
	if !captured.Enabled || captured.Installation.Suspended || len(captured.Bindings) != 1 || captured.Bindings[0] != grant.fixture.source.Bindings[0] {
		return nil, domain.ErrForbidden
	}
	return func(write context.Context) error {
		grant.fences.Add(1)
		if !grant.fixture.postgres.InTransaction(write) || grant.fixture.postgres.InReadOnlyTransaction(write) {
			return domain.ErrIntegrity
		}
		current, err := grant.fixture.postgres.GetGitHubConnection(write, captured.NamespaceID)
		if err != nil {
			return err
		}
		if !current.Enabled || current.Generation != captured.Generation || !reflect.DeepEqual(current.Bindings, captured.Bindings) {
			return domain.ErrForbidden
		}
		return nil
	}, nil
}

func wave42PGGitOwner(test *testing.T, fixture *wave42PGGitPGFixture, metadata outbound.MetadataStore, reader outbound.GitEvidenceReader, authorizer outbound.GitSourceAuthorizer) *gitchangeworker.Worker {
	test.Helper()
	worker, err := NewGitChangeWorker(GitChangeWorkerConfig{
		Profile: gitchangeworker.Complete, Metadata: metadata, Blobs: fixture.postgres,
		Repositories: fixture.postgres, Reader: reader, Authorizer: authorizer,
	})
	if err != nil || worker == nil {
		test.Fatal("public complete factory rejected owned PostgreSQL adapters", err)
	}
	return worker
}

func TestPGWave42GitChangeFactoryDurableRetryRestartAndExactResult(test *testing.T) {
	fixture := wave42PGGitPG(test)
	queued := fixture.enqueue(test)
	ctx := inbound.WithDocumentIdentities(context.Background(), []domain.DocumentIdentity{"caller-spoof"})
	storage := &wave42PGGitWorkerPGStore{PostgresStore: fixture.postgres}
	providerFailure := errors.New("local fake provider unavailable")
	reader := &wave42PGGitReader{fixture: fixture, failure: providerFailure}
	grant := &wave42PGGitGrantPG{fixture: fixture}
	worker := wave42PGGitOwner(test, fixture, storage, reader, grant)
	if err := worker.Process(ctx, 1); err != nil {
		test.Fatal("Process changed the original deferred-job error contract", err)
	}
	retrying := fixture.job(test, queued.ID)
	if retrying.State != "retrying" || retrying.Result != nil || retrying.Attempts != 1 || retrying.Version != 1 || retrying.Reason != "temporary_provider_or_storage_failure" || !retrying.LeaseUntil.IsZero() || !retrying.NextAttempt.Equal(retrying.UpdatedAt.Add(2*time.Second)) {
		test.Fatal("native worker changed the original retry transition", retrying)
	}
	fixture.postgres.Close()
	reopened, err := store.NewPostgresStore(context.Background(), os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(reopened.Close)
	fixture.postgres = reopened
	if recovered := fixture.job(test, queued.ID); !reflect.DeepEqual(recovered, retrying) {
		test.Fatal("restart changed the durable retry payload", recovered, retrying)
	}
	if err := reopened.RetryGitChange(wave40PGContext(context.Background(), reopened), queued.RepoID, queued.ID, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	storage = &wave42PGGitWorkerPGStore{PostgresStore: reopened}
	reader = &wave42PGGitReader{fixture: fixture}
	grant = &wave42PGGitGrantPG{fixture: fixture}
	worker = wave42PGGitOwner(test, fixture, storage, reader, grant)
	before, err := reopened.RepositoryRevision(context.Background(), queued.RepoID)
	if err != nil {
		test.Fatal(err)
	}
	if err := worker.Process(ctx, 1); err != nil {
		test.Fatal(err)
	}
	done := fixture.job(test, queued.ID)
	expected, err := domain.AssessGitReversal(fixture.deltas[fixture.request.Target], fixture.deltas[fixture.request.Commit])
	if err != nil || done.Result == nil || !reflect.DeepEqual(*done.Result, expected) || done.State != "completed" || done.Version != 3 || done.Attempts != 2 || done.Reason != expected.Reason || done.ID != queued.ID || done.Request != queued.Request || !done.CreatedAt.Equal(queued.CreatedAt) {
		test.Fatal("standalone worker changed immutable identity or exact reversal meaning", done, expected, err)
	}
	after, err := reopened.RepositoryRevision(context.Background(), queued.RepoID)
	if err != nil || after.Graph != before.Graph || after.Pending != before.Pending || after.Evidence != before.Evidence+1 || storage.reads.Load() != 1 || storage.cacheReads.Load() < 3 || reader.reads.Load() != 2 || reader.ancestry.Load() != 1 || grant.checks.Load() != 1 || grant.fences.Load() != 1 {
		test.Fatal("worker lost original snapshot/cache/provider/fence/revision boundaries", before, after, err)
	}
	if err := worker.Process(ctx, 4); err != nil || !reflect.DeepEqual(fixture.job(test, queued.ID), done) || reader.reads.Load() != 2 {
		test.Fatal("terminal job was reclaimed or reverified", err)
	}
	if replay := fixture.enqueue(test); !reflect.DeepEqual(replay, done) {
		test.Fatal("idempotent acceptance changed the original Git change ID or terminal payload", replay, done)
	}
	for _, table := range []string{"refs", "context_history", "git_ref_observations"} {
		var count int
		if err := fixture.observer.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE repo_id=$1", queued.RepoID).Scan(&count); err != nil || count != 0 {
			test.Fatal("Git evidence processing changed context history or refs", table, count, err)
		}
	}
}

func TestPGWave42GitChangeFactoryCurrentGrantAndMetadataOriginFence(test *testing.T) {
	for _, mutation := range []string{"origin", "grant-generation", "grant-revoked"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := wave42PGGitPG(test)
			queued := fixture.enqueue(test)
			storage := &wave42PGGitWorkerPGStore{PostgresStore: fixture.postgres}
			grant := &wave42PGGitGrantPG{fixture: fixture}
			reader := &wave42PGGitReader{fixture: fixture}
			policyOracle := NewGitHubConnections(fixture.team.identity, NewService(fixture.postgres, fixture.postgres, nil, nil, fixture.postgres), fixture.postgres, &githubFake{})
			oracleFence, err := policyOracle.AuthorizeGitRead(context.Background(), fixture.repo.ID)
			if err != nil {
				test.Fatal("original source-grant oracle rejected the fixture", err)
			}
			reader.afterRead = func(ctx context.Context) error {
				if mutation == "origin" {
					changed, err := fixture.observer.Exec(ctx, `UPDATE repos SET git_remote_url=$2 WHERE id=$1`, fixture.repo.ID, "https://github.com/acme/rebound.git")
					if err == nil && changed.RowsAffected() != 1 {
						return domain.ErrIntegrity
					}
					return err
				}
				return fixture.postgres.WithinIdentity(ctx, func(bound context.Context) error {
					current, err := fixture.postgres.GetGitHubConnection(bound, fixture.source.NamespaceID)
					if err != nil {
						return err
					}
					if mutation == "grant-generation" {
						current.Generation++
					} else {
						current.Enabled = false
					}
					return fixture.postgres.PutGitHubConnection(bound, current)
				})
			}
			claim, err := fixture.postgres.ClaimGitChange(wave40PGContext(context.Background(), fixture.postgres), queued.RepoID, queued.ID, time.Now().UTC(), 2*time.Minute)
			if err != nil {
				test.Fatal(err)
			}
			worker := wave42PGGitOwner(test, fixture, storage, reader, grant)
			err = worker.RunLease(wave40PGContext(context.Background(), fixture.postgres), claim)
			want := error(domain.ErrForbidden)
			state, reason, fences := "retrying", "temporary_provider_or_storage_failure", int64(1)
			if mutation == "origin" {
				want, state, reason, fences = domain.ErrConflict, "attention", "origin_or_lease_changed", 0
			}
			current := fixture.job(test, queued.ID)
			if !errors.Is(err, want) || current.State != state || current.Reason != reason || current.Result != nil || current.Version != claim.Version || current.Attempts != claim.Attempts || grant.checks.Load() != 1 || grant.fences.Load() != fences {
				test.Fatal("post-provider current source or origin fence was bypassed", current, err, grant.fences.Load())
			}
			if err := fixture.postgres.WithinRepository(wave40PGContext(context.Background(), fixture.postgres), fixture.repo.ID, oracleFence); !errors.Is(err, want) {
				test.Fatal("standalone source fence diverged from the original current-policy oracle", err, want)
			}
		})
	}
}

func wave42PGWaitSQLBlock(ctx context.Context, transaction pgx.Tx, queryPart string) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := transaction.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			return err
		}
		var blocked bool
		err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND pg_backend_pid()=ANY(pg_blocking_pids(pid)) AND position($1 in query)>0)`, queryPart).Scan(&blocked)
		if err != nil || blocked {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func wave42PGRollback(transaction pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = transaction.Rollback(ctx)
}

func TestPGWave42GitChangeFactoryRechecksOriginAfterSQLLockBarrier(test *testing.T) {
	fixture := wave42PGGitPG(test)
	queued := fixture.enqueue(test)
	ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), fixture.postgres), 15*time.Second)
	defer cancel()
	claim, err := fixture.postgres.ClaimGitChange(ctx, queued.RepoID, queued.ID, time.Now().UTC(), 2*time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	blocker, err := fixture.observer.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer wave42PGRollback(blocker)
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), 1735289201)`, string(queued.RepoID)); err != nil {
		test.Fatal(err)
	}
	storage := &wave42PGGitWorkerPGStore{PostgresStore: fixture.postgres}
	reader := &wave42PGGitReader{fixture: fixture}
	grant := &wave42PGGitGrantPG{fixture: fixture}
	worker := wave42PGGitOwner(test, fixture, storage, reader, grant)
	finished := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		finished <- worker.RunLease(ctx, claim)
	}()
	defer func() {
		cancel()
		wave42PGRollback(blocker)
		<-joined
	}()
	if err := wave42PGWaitSQLBlock(ctx, blocker, "pg_advisory_xact_lock(hashtext"); err != nil {
		test.Fatal("worker did not reach the original repository SQL lock", err)
	}
	if reader.reads.Load() != 2 || reader.ancestry.Load() != 1 || grant.checks.Load() != 1 || grant.fences.Load() != 0 {
		test.Fatal("provider/fence ordering changed at the observed DB lock barrier")
	}
	if _, err := blocker.Exec(ctx, `UPDATE repos SET git_remote_url=$2 WHERE id=$1`, queued.RepoID, "https://github.com/acme/late-binding.git"); err != nil {
		test.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	err = <-finished
	current := fixture.job(test, queued.ID)
	if !errors.Is(err, domain.ErrConflict) || current.State != "attention" || current.Result != nil || current.Reason != "origin_or_lease_changed" || grant.fences.Load() != 0 {
		test.Fatal("publication used metadata captured before its real SQL wait", current, err)
	}
}

func TestPGWave42GitChangeFactoryConcurrentClaimRecoveryAndStaleVersion(test *testing.T) {
	fixture := wave42PGGitPG(test)
	queued := fixture.enqueue(test)
	ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), fixture.postgres), 15*time.Second)
	defer cancel()
	storage := &wave42PGGitWorkerPGStore{PostgresStore: fixture.postgres}
	release := make(chan struct{})
	reader := &wave42PGGitReader{fixture: fixture, entered: make(chan struct{}, 1), release: release}
	first := wave42PGGitOwner(test, fixture, storage, reader, nil)
	second := wave42PGGitOwner(test, fixture, storage, reader, nil)
	finished := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		finished <- first.Process(ctx, 1)
	}()
	defer func() {
		cancel()
		<-joined
	}()
	select {
	case <-reader.entered:
	case <-ctx.Done():
		test.Fatal("first owner never reached local provider", ctx.Err())
	}
	if err := second.Process(ctx, 1); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("shared admission did not reject concurrent heavy work", err)
	}
	if _, err := fixture.postgres.ClaimGitChange(ctx, queued.RepoID, queued.ID, time.Now().UTC(), 2*time.Minute); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("another claimant acquired the live durable lease", err)
	}
	running := fixture.job(test, queued.ID)
	storage.mutex.Lock()
	leases := append([]domain.GitChangeJob(nil), storage.leases...)
	storage.mutex.Unlock()
	if len(leases) != 1 || running.State != "running" || running.Version != 1 || running.Attempts != 1 || storage.claims.Load() != 1 {
		test.Fatal("two live public workers claimed the same durable version", running, leases)
	}
	recovered, err := fixture.postgres.ClaimGitChange(ctx, queued.RepoID, queued.ID, running.LeaseUntil.Add(time.Second), 2*time.Minute)
	if err != nil || recovered.Version != running.Version+1 || recovered.Attempts != running.Attempts+1 {
		test.Fatal("expired durable lease did not advance the original fence", recovered, err)
	}
	recoveryReader := &wave42PGGitReader{fixture: fixture}
	restarted := wave42PGGitOwner(test, fixture, storage, recoveryReader, nil)
	if err := restarted.RunLease(ctx, recovered); err != nil {
		test.Fatal(err)
	}
	done := fixture.job(test, queued.ID)
	close(release)
	err = <-finished
	if err != nil || done.State != "completed" || done.Version != recovered.Version || !reflect.DeepEqual(fixture.job(test, queued.ID), done) {
		test.Fatal("stale Process completion overwrote the recovered terminal row", done, err)
	}
	if err := first.RunLease(ctx, leases[0]); !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(fixture.job(test, queued.ID), done) {
		test.Fatal("explicit stale RunLease bypassed the durable version fence", err)
	}
}

type wave42PGRepositoryPGStore struct {
	*store.PostgresStore
	rejectRevision error
	rejectAudit    error
	commitACK      error
	readACK        error
	writes         int
}

func (storage *wave42PGRepositoryPGStore) WithinRepository(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	storage.writes++
	err := storage.PostgresStore.WithinRepository(ctx, repo, write)
	if err == nil && storage.commitACK != nil {
		err, storage.commitACK = storage.commitACK, nil
	}
	return err
}

func (storage *wave42PGRepositoryPGStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	err := storage.PostgresStore.WithinReadSnapshot(ctx, read)
	if err == nil && storage.readACK != nil {
		return storage.readACK
	}
	return err
}

func (storage *wave42PGRepositoryPGStore) AdvanceRepositoryRevision(ctx context.Context, repo domain.ContentHash, pending bool) error {
	if err := storage.PostgresStore.AdvanceRepositoryRevision(ctx, repo, pending); err != nil {
		return err
	}
	return storage.rejectRevision
}

func (storage *wave42PGRepositoryPGStore) AdvanceEvidenceRevision(ctx context.Context, repo domain.ContentHash) error {
	if err := storage.PostgresStore.AdvanceEvidenceRevision(ctx, repo); err != nil {
		return err
	}
	return storage.rejectRevision
}

func (storage *wave42PGRepositoryPGStore) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if err := storage.PostgresStore.AppendOrganizationAudit(ctx, event); err != nil {
		return err
	}
	return storage.rejectAudit
}

type wave42PGRepositoryOutcome struct {
	branch    string
	revision  domain.RepositoryRevision
	audits    int
	callbacks int
	attempts  int
	result    string
	failed    bool
}

func wave42PGRepositoryModel(test *testing.T, native bool) []wave42PGRepositoryOutcome {
	test.Helper()
	fixture := wave42PGGitPG(test)
	storage := &wave42PGRepositoryPGStore{PostgresStore: fixture.postgres}
	supported := ReadDocumentIdentities(storage, fixture.postgres)
	work := newRepositoryWork(storage, storage, supported)
	oracle := NewService(storage, fixture.postgres, nil, nil, storage)
	write := func(ctx context.Context, body func(context.Context) (string, error)) (string, error) {
		if native {
			return repositoryWriteWithWork(ctx, work, fixture.repo.ID, body)
		}
		return repositoryWrite(ctx, oracle, fixture.repo.ID, body)
	}
	ctx := inbound.WithRepositoryActor(context.Background(), fixture.team.owner.ID)
	ctx = inbound.WithDocumentIdentities(ctx, supported)
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, supported, supported)
	failure := errors.New("wave42PG transaction failure")
	unknownACK := errors.New("wave42PG committed with acknowledgement lost")
	cases := []struct {
		name          string
		scope         string
		failure       string
		nested        bool
		abortCode     string
		wantAttempts  int
		wantCommitted bool
	}{
		{name: "graph", wantAttempts: 1, wantCommitted: true},
		{name: "pending", scope: "pending", wantAttempts: 1, wantCommitted: true},
		{name: "evidence", scope: "evidence", wantAttempts: 1, wantCommitted: true},
		{name: "none", scope: "none", wantAttempts: 1, wantCommitted: true},
		{name: "nested", nested: true, wantAttempts: 1, wantCommitted: true},
		{name: "body-rollback", failure: "body", wantAttempts: 1},
		{name: "revision-rollback", scope: "evidence", failure: "revision", wantAttempts: 1},
		{name: "audit-rollback", failure: "audit", wantAttempts: 1},
		{name: "serialization", abortCode: "40001", wantAttempts: 2, wantCommitted: true},
		{name: "deadlock", abortCode: "40P01", wantAttempts: 2, wantCommitted: true},
		{name: "retry-cap", failure: "retry-cap", abortCode: "40001", wantAttempts: 3},
		{name: "unknown-ack", failure: "ack", wantAttempts: 1, wantCommitted: true},
	}
	var outcomes []wave42PGRepositoryOutcome
	for _, scenario := range cases {
		before, err := fixture.postgres.GetRepo(ctx, fixture.repo.ID)
		if err != nil {
			test.Fatal(err)
		}
		revision, err := fixture.postgres.RepositoryRevision(ctx, fixture.repo.ID)
		if err != nil {
			test.Fatal(err)
		}
		if scenario.failure == "revision" {
			storage.rejectRevision = failure
		}
		if scenario.failure == "audit" {
			storage.rejectAudit = failure
		}
		if scenario.failure == "ack" {
			storage.commitACK = unknownACK
		}
		correlation := "wave42PG-model-" + scenario.name
		bound := inbound.WithCorrelation(ctx, correlation)
		bound = writeAction(auditOperation(bound, "wave42PG.repository.updated"), "manage")
		bound = context.WithValue(bound, revisionScopeKey{}, scenario.scope)
		attempts, callbacks := 0, 0
		operation := func(transaction context.Context) (string, error) {
			attempts++
			if !fixture.postgres.InTransaction(transaction) || fixture.postgres.InReadOnlyTransaction(transaction) || transaction.Value(afterCommitKey{}) == nil || transaction.Value(writeActionKey{}) != "manage" || transaction.Value(auditOperationKey{}) != "wave42PG.repository.updated" || transaction.Value(revisionScopeKey{}) != scenario.scope || inbound.Correlation(transaction) != correlation {
				return "", domain.ErrIntegrity
			}
			nextBranch := "wave42PG-" + scenario.name
			if err := fixture.postgres.UpdateRepoConfig(transaction, fixture.repo.ID, &nextBranch, nil); err != nil {
				return "", err
			}
			register := func(current context.Context) (string, error) {
				afterRepositoryCommit(current, func() {
					callbacks++
					var observed string
					if err := fixture.observer.QueryRow(context.Background(), `SELECT default_branch FROM repos WHERE id=$1`, fixture.repo.ID).Scan(&observed); err != nil || observed != nextBranch {
						test.Error("after-commit callback ran before durable publication", observed, err)
					}
				})
				return "published", nil
			}
			var result string
			var err error
			if scenario.nested {
				result, err = write(transaction, register)
			} else {
				result, err = register(transaction)
			}
			if err != nil || callbacks != 0 {
				return "", errors.Join(err, domain.ErrIntegrity)
			}
			if scenario.failure == "body" {
				return result, failure
			}
			if scenario.abortCode != "" && (attempts == 1 || scenario.failure == "retry-cap") {
				return result, &pgconn.PgError{Code: scenario.abortCode, Message: "explicit transaction abort"}
			}
			return result, nil
		}
		result, err := write(bound, operation)
		storage.rejectRevision, storage.rejectAudit = nil, nil
		current, readErr := fixture.postgres.GetRepo(ctx, fixture.repo.ID)
		if readErr != nil {
			test.Fatal(readErr)
		}
		after, readErr := fixture.postgres.RepositoryRevision(ctx, fixture.repo.ID)
		if readErr != nil {
			test.Fatal(readErr)
		}
		events, readErr := fixture.postgres.ListOrganizationAudit(ctx, fixture.team.organization.ID, 500)
		if readErr != nil {
			test.Fatal(readErr)
		}
		audits := 0
		for _, event := range events {
			if event.CorrelationID != correlation {
				continue
			}
			audits++
			if event.ActorID != fixture.team.owner.ID || event.Action != "wave42PG.repository.updated" || event.TargetType != "repository" || event.TargetID != fixture.team.repository.ID || event.Reason != "" {
				test.Fatal("original metadata-only audit semantics changed", event)
			}
		}
		wantBranch, wantRevision, wantAudits, wantCallbacks := before.DefaultBranch, revision, 0, 0
		if scenario.wantCommitted {
			wantBranch, wantAudits = "wave42PG-"+scenario.name, 1
			switch scenario.scope {
			case "none":
			case "evidence":
				wantRevision.Evidence++
			case "pending":
				wantRevision.Pending++
			default:
				wantRevision.Graph++
			}
			if scenario.failure != "ack" {
				wantCallbacks = 1
			}
		}
		wantFailure := scenario.failure != ""
		wantResult := "published"
		if wantFailure {
			wantResult = ""
		}
		if current.DefaultBranch != wantBranch || after != wantRevision || audits != wantAudits || callbacks != wantCallbacks || attempts != scenario.wantAttempts || (err != nil) != wantFailure || result != wantResult {
			test.Fatal("shared work diverged from exact transaction model", native, scenario.name, current.DefaultBranch, after, audits, callbacks, attempts, result, err)
		}
		if scenario.failure == "ack" && !errors.Is(err, unknownACK) || (scenario.failure == "body" || scenario.failure == "revision" || scenario.failure == "audit") && !errors.Is(err, failure) {
			test.Fatal("shared work changed the original error identity", scenario.name, err)
		}
		if scenario.failure == "retry-cap" {
			var abort *pgconn.PgError
			if !errors.As(err, &abort) || abort.Code != scenario.abortCode {
				test.Fatal("retry cap lost the original SQLSTATE", err)
			}
		}
		outcomes = append(outcomes, wave42PGRepositoryOutcome{branch: current.DefaultBranch, revision: after, audits: audits, callbacks: callbacks, attempts: attempts, result: result, failed: err != nil})
	}
	return outcomes
}

func TestPGWave42SharedRepositoryWorkMatchesOriginalTransactionModel(test *testing.T) {
	var original, native []wave42PGRepositoryOutcome
	test.Run("original-service-oracle", func(test *testing.T) { original = wave42PGRepositoryModel(test, false) })
	test.Run("standalone-repository-work", func(test *testing.T) { native = wave42PGRepositoryModel(test, true) })
	if !reflect.DeepEqual(original, native) {
		test.Fatal("root thin entry point and standalone work changed revision/audit/rollback/ACK semantics", original, native)
	}
}

func TestPGWave42SharedRepositoryWorkRechecksCurrentAuthorityAfterSQLBarrier(test *testing.T) {
	fixture := wave42PGGitPG(test)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := fixture.postgres.AddMember(ctx, domain.Membership{RepositoryID: fixture.team.repository.ID, UserID: fixture.team.member.ID, Role: domain.RoleMember}); err != nil {
		test.Fatal(err)
	}
	actor := inbound.WithRepositoryActor(ctx, fixture.team.member.ID)
	work := newRepositoryWork(fixture.postgres, fixture.postgres, ReadDocumentIdentities(fixture.postgres, fixture.postgres))
	oracle := NewService(fixture.postgres, fixture.postgres, nil, nil, fixture.postgres)
	if err := fixture.postgres.WithinRepository(actor, fixture.repo.ID, func(bound context.Context) error {
		if err := work.authorizeRepositoryWrite(bound, fixture.repo.ID); err != nil {
			return err
		}
		return oracle.authorizeRepositoryWrite(bound, fixture.repo.ID)
	}); err != nil {
		test.Fatal("valid direct-member fixture was not authorized", err)
	}
	before, err := fixture.postgres.RepositoryRevision(ctx, fixture.repo.ID)
	if err != nil {
		test.Fatal(err)
	}
	blocker, err := fixture.observer.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer wave42PGRollback(blocker)
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cxt-identity-access',0))`); err != nil {
		test.Fatal(err)
	}
	var bodies atomic.Int64
	finished, joined := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(joined)
		finished <- repositoryWriteErrorWithWork(actor, work, fixture.repo.ID, func(context.Context) error {
			bodies.Add(1)
			return nil
		})
	}()
	defer func() {
		cancel()
		wave42PGRollback(blocker)
		<-joined
	}()
	if err := wave42PGWaitSQLBlock(ctx, blocker, "pg_advisory_xact_lock_shared"); err != nil {
		test.Fatal("repository work did not wait on the original identity SQL lock", err)
	}
	if bodies.Load() != 0 {
		test.Fatal("write body ran before current authority was pinned")
	}
	changed, err := blocker.Exec(ctx, `DELETE FROM memberships WHERE repository_id=$1 AND user_id=$2`, fixture.team.repository.ID, fixture.team.member.ID)
	if err != nil || changed.RowsAffected() != 1 {
		test.Fatal("owned member revocation did not affect exactly one grant", err)
	}
	if err := blocker.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if err := <-finished; !errors.Is(err, domain.ErrForbidden) || bodies.Load() != 0 {
		test.Fatal("pre-lock authority was reused after committed revocation", err, bodies.Load())
	}
	if err := fixture.postgres.WithinRepository(actor, fixture.repo.ID, func(bound context.Context) error {
		return oracle.authorizeRepositoryWrite(bound, fixture.repo.ID)
	}); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("native denial changed the original current-authority oracle", err)
	}
	after, err := fixture.postgres.RepositoryRevision(ctx, fixture.repo.ID)
	if err != nil || before != after {
		test.Fatal("denied work advanced a durable revision", before, after, err)
	}
}

func TestPGWave42SharedRepositoryReadPinsSnapshotAndPreservesBorrowedWrite(test *testing.T) {
	fixture := wave42PGGitPG(test)
	ctx := wave40PGContext(context.Background(), fixture.postgres)
	storage := &wave42PGRepositoryPGStore{PostgresStore: fixture.postgres}
	work := newRepositoryWork(storage, fixture.postgres, ReadDocumentIdentities(storage, fixture.postgres))
	oracle := NewService(storage, fixture.postgres, nil, nil, fixture.postgres)
	for _, native := range []bool{false, true} {
		read := func(ctx context.Context, body func(context.Context) (domain.Repo, error)) (domain.Repo, error) {
			if native {
				return repositoryReadWithWork(ctx, work, body)
			}
			return repositoryRead(ctx, oracle, body)
		}
		before, err := fixture.postgres.GetRepo(ctx, fixture.repo.ID)
		if err != nil {
			test.Fatal(err)
		}
		nextBranch := fmt.Sprintf("snapshot-%t", native)
		pinned, err := read(ctx, func(bound context.Context) (domain.Repo, error) {
			if !fixture.postgres.InReadOnlyTransaction(bound) {
				return domain.Repo{}, domain.ErrIntegrity
			}
			first, err := fixture.postgres.GetRepo(bound, fixture.repo.ID)
			if err != nil {
				return domain.Repo{}, err
			}
			if _, err := fixture.observer.Exec(ctx, `UPDATE repos SET default_branch=$2 WHERE id=$1`, fixture.repo.ID, nextBranch); err != nil {
				return domain.Repo{}, err
			}
			second, err := fixture.postgres.GetRepo(bound, fixture.repo.ID)
			if err == nil && !reflect.DeepEqual(first, second) {
				return domain.Repo{}, domain.ErrIntegrity
			}
			return second, err
		})
		if err != nil || !reflect.DeepEqual(pinned, before) {
			test.Fatal("repository read lost the original coherent snapshot", native, pinned, before, err)
		}
		failure := errors.New("read completion acknowledgement unknown")
		storage.readACK = failure
		if result, err := read(ctx, func(bound context.Context) (domain.Repo, error) {
			return fixture.postgres.GetRepo(bound, fixture.repo.ID)
		}); !errors.Is(err, failure) || !reflect.DeepEqual(result, domain.Repo{}) {
			test.Fatal("failed read completion returned a success-shaped value", native, result, err)
		}
		storage.readACK = nil
		err = fixture.postgres.WithinRepository(ctx, fixture.repo.ID, func(bound context.Context) error {
			branch := "borrowed-write"
			if err := fixture.postgres.UpdateRepoConfig(bound, fixture.repo.ID, &branch, nil); err != nil {
				return err
			}
			current, err := read(bound, func(nested context.Context) (domain.Repo, error) {
				if !fixture.postgres.InTransaction(nested) || fixture.postgres.InReadOnlyTransaction(nested) {
					return domain.Repo{}, domain.ErrIntegrity
				}
				return fixture.postgres.GetRepo(nested, fixture.repo.ID)
			})
			if err != nil || current.DefaultBranch != branch {
				return errors.Join(err, domain.ErrIntegrity)
			}
			return failure
		})
		current, readErr := fixture.postgres.GetRepo(ctx, fixture.repo.ID)
		if !errors.Is(err, failure) || readErr != nil || current.DefaultBranch != nextBranch {
			test.Fatal("nested repository read independently committed its borrowed write", native, current, err, readErr)
		}
	}
}

func TestPGWave42GitChangeFactoryLostCommitACKKeepsTerminalEvidence(test *testing.T) {
	fixture := wave42PGGitPG(test)
	queued := fixture.enqueue(test)
	ctx := wave40PGContext(context.Background(), fixture.postgres)
	claim, err := fixture.postgres.ClaimGitChange(ctx, queued.RepoID, queued.ID, time.Now().UTC(), 2*time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	unknownACK := errors.New("Git change result committed but acknowledgement lost")
	storage := &wave42PGRepositoryPGStore{PostgresStore: fixture.postgres, commitACK: unknownACK}
	reader := &wave42PGGitReader{fixture: fixture}
	worker := wave42PGGitOwner(test, fixture, storage, reader, nil)
	before, err := fixture.postgres.RepositoryRevision(ctx, queued.RepoID)
	if err != nil {
		test.Fatal(err)
	}
	err = worker.RunLease(ctx, claim)
	if !errors.Is(err, unknownACK) || !errors.Is(err, domain.ErrConflict) || storage.writes != 2 {
		test.Fatal("unknown publication ACK was retried as an aborted transaction or stale failure was accepted", storage.writes, err)
	}
	done := fixture.job(test, queued.ID)
	after, err := fixture.postgres.RepositoryRevision(ctx, queued.RepoID)
	if err != nil || done.State != "completed" || done.Result == nil || done.Version != claim.Version || done.Attempts != claim.Attempts || after.Evidence != before.Evidence+1 || after.Graph != before.Graph || after.Pending != before.Pending {
		test.Fatal("unknown ACK lost the one durable terminal publication", done, before, after, err)
	}
	restarted := wave42PGGitOwner(test, fixture, fixture.postgres, &wave42PGGitReader{fixture: fixture}, nil)
	if err := restarted.Process(ctx, 1); err != nil || !reflect.DeepEqual(fixture.job(test, queued.ID), done) {
		test.Fatal("restart after unknown ACK overwrote the terminal proof", err)
	}
	if err := fixture.postgres.RetryGitChange(ctx, queued.RepoID, queued.ID, time.Now().UTC()); err != nil || !reflect.DeepEqual(fixture.job(test, queued.ID), done) {
		test.Fatal("idempotent terminal retry changed the completed row", err)
	}
}

func TestPGWave42GitChangeFactoryReadsExistingImmutableCacheBeforeProvider(test *testing.T) {
	fixture := wave42PGGitPG(test)
	queued := fixture.enqueue(test)
	for _, delta := range fixture.deltas {
		record := domain.NewGitDelta(queued.RepoID, queued.GitOrigin, delta)
		if err := record.Validate(); err != nil {
			test.Fatal(err)
		}
		payload, err := json.Marshal(record)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := fixture.observer.Exec(context.Background(), `INSERT INTO git_commit_deltas(repo_id,id,origin,commit_oid,parent_oid,payload,keys,inverse_keys) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, record.RepoID, record.ID, record.GitOrigin, record.Delta.Commit, record.Delta.Parent, payload, record.Keys, record.InverseKeys); err != nil {
			test.Fatal(err)
		}
	}
	storage := &wave42PGGitWorkerPGStore{PostgresStore: fixture.postgres}
	reader := &wave42PGGitReader{fixture: fixture}
	worker := wave42PGGitOwner(test, fixture, storage, reader, nil)
	if err := worker.Process(context.Background(), 1); err != nil {
		test.Fatal(err)
	}
	done := fixture.job(test, queued.ID)
	if done.State != "completed" || done.Result == nil || done.Result.Coverage != "full" || reader.reads.Load() != 0 || reader.ancestry.Load() != 1 || storage.cacheReads.Load() != 3 {
		test.Fatal("cached deltas or unknown ancestry changed original fallback policy", done, reader.reads.Load(), reader.ancestry.Load(), storage.cacheReads.Load())
	}
}

func TestPGWave42GitChangeFactoryCurrentDocumentIdentityAtClaimAndPublication(test *testing.T) {
	for _, mode := range []string{"complete-root", "compatibility-root", "compatibility-late-root"} {
		test.Run(mode, func(test *testing.T) {
			fixture := wave42PGGitPG(test)
			queued := fixture.enqueue(test)
			if mode != "compatibility-late-root" {
				if err := fixture.postgres.RequireDocumentIdentity(wave40PGContext(context.Background(), fixture.postgres), queued.RepoID, domain.DocumentIdentityRootV1); err != nil {
					test.Fatal(err)
				}
			}
			reader := &wave42PGGitReader{fixture: fixture}
			config := GitChangeWorkerConfig{
				Profile: gitchangeworker.Compatibility, Metadata: fixture.postgres,
				Repositories: fixture.postgres, Reader: reader,
			}
			if mode == "complete-root" {
				config.Profile, config.Blobs = gitchangeworker.Complete, fixture.postgres
			}
			if mode == "compatibility-late-root" {
				reader.afterRead = func(ctx context.Context) error {
					return fixture.postgres.RequireDocumentIdentity(ctx, queued.RepoID, domain.DocumentIdentityRootV1)
				}
			}
			worker, err := NewGitChangeWorker(config)
			if err != nil || worker == nil {
				test.Fatal("identity fixture could not assemble the specified profile", err)
			}
			spoofed := inbound.WithDocumentIdentities(context.Background(), []domain.DocumentIdentity{"caller-spoof"})
			spoofed = outbound.WithDocumentIdentityCompatibility(spoofed, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
			if err := worker.Process(spoofed, 1); err != nil {
				test.Fatal("Process lost its deferred-job error semantics", err)
			}
			current := fixture.job(test, queued.ID)
			switch mode {
			case "complete-root":
				if current.State != "completed" || current.Result == nil || current.Version != 1 || current.Attempts != 1 {
					test.Fatal("complete owner lost its own binary root declaration", current)
				}
			case "compatibility-root":
				if !reflect.DeepEqual(current, queued) || reader.reads.Load() != 0 {
					test.Fatal("legacy-only owner spent an unsupported claim or inherited caller support", current)
				}
			case "compatibility-late-root":
				if current.State != "running" || current.Result != nil || current.Version != 1 || current.Attempts != 1 || reader.reads.Load() != 2 {
					test.Fatal("late root requirement was bypassed during publication or cleanup", current)
				}
			}
		})
	}
}

type wave42PGGitMetadataOnly struct {
	outbound.MetadataStore
}

type wave42PGGitMetadataWithoutTransactions struct {
	outbound.MetadataStore
	outbound.GitChangeStore
}

type wave42PGGitMetadataWithoutJobs struct {
	outbound.MetadataStore
	outbound.RepositoryTransactions
}

func TestPGWave42GitChangeFactoryValidatesCompleteAndOriginalCompatibility(test *testing.T) {
	fixture := wave42PGGitPG(test)
	reader := &wave42PGGitReader{fixture: fixture}
	valid := GitChangeWorkerConfig{
		Profile: gitchangeworker.Complete, Metadata: fixture.postgres, Blobs: fixture.postgres,
		Repositories: fixture.postgres, Reader: reader,
	}
	var nilPostgres *store.PostgresStore
	var nilReader *wave42PGGitReader
	var nilAuthorizer *wave42PGGitGrantPG
	for _, missing := range []string{"profile", "metadata", "blobs", "repositories", "reader", "transactions", "jobs", "typed-metadata", "typed-blobs", "typed-repositories", "typed-reader", "typed-authorizer"} {
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
			case "transactions":
				config.Metadata = wave42PGGitMetadataWithoutTransactions{MetadataStore: fixture.postgres, GitChangeStore: fixture.postgres}
			case "jobs":
				config.Metadata = wave42PGGitMetadataWithoutJobs{MetadataStore: fixture.postgres, RepositoryTransactions: fixture.postgres}
			case "typed-metadata":
				config.Metadata = nilPostgres
			case "typed-blobs":
				config.Blobs = nilPostgres
			case "typed-repositories":
				config.Repositories = nilPostgres
			case "typed-reader":
				config.Reader = nilReader
			case "typed-authorizer":
				config.Authorizer = nilAuthorizer
			}
			if worker, err := NewGitChangeWorker(config); err == nil || worker != nil {
				test.Fatal("complete factory accepted an absent/typed-nil required capability", missing, worker, err)
			}
		})
	}
	local := store.NewFSStore(test.TempDir())
	if _, err := local.PutRepo(systemTestContext(), fixture.repo); err != nil {
		test.Fatal(err)
	}
	compatibility, err := NewGitChangeWorker(GitChangeWorkerConfig{
		Profile: gitchangeworker.Compatibility, Metadata: local, Repositories: local, Reader: reader,
	})
	if err != nil || compatibility == nil {
		test.Fatal("original FS/nil-blob compatibility was rejected", err)
	}
	if err := compatibility.Process(context.Background(), 0); err != nil {
		test.Fatal("zero-limit compatibility did work", err)
	}
	metadataOnly := wave42PGGitMetadataOnly{MetadataStore: fixture.postgres}
	oracle := NewService(metadataOnly, nil, nil, nil, nil)
	_, originalErr := NewGitChanges(oracle, nil)
	_, nativeErr := NewGitChangeWorker(GitChangeWorkerConfig{Profile: gitchangeworker.Compatibility, Metadata: metadataOnly})
	if originalErr == nil || originalErr.Error() != "durable Git change verification unavailable" || !errors.Is(nativeErr, domain.ErrValidation) {
		test.Fatal("legacy error contract or explicit factory validation changed", nativeErr, originalErr)
	}
	storage := &wave42PGGitWorkerPGStore{PostgresStore: fixture.postgres}
	worker := wave42PGGitOwner(test, fixture, storage, reader, nil)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := worker.Process(canceled, 1); !errors.Is(err, context.Canceled) || storage.claims.Load() != 0 {
		test.Fatal("canceled Process spent a claim or lost cancellation", err)
	}
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		worker.Run(canceled)
	}()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		test.Fatal("canceled Run did not join")
	}
	if storage.claims.Load() != 0 || reader.reads.Load() != 0 {
		test.Fatal("canceled Run reached claims or provider after join")
	}
}
