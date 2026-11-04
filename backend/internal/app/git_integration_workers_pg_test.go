//go:build postgres

package app

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func wave43PGObserver(test *testing.T, dsn string) *pgx.Conn {
	test.Helper()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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
	return observer
}

func wave43PGOwner(test *testing.T, metadata outbound.MetadataStore, blobs outbound.BlobStore, repositories outbound.RepositoryStore, reader outbound.GitCommitReader, source outbound.GitSourceAuthorizer) *GitIntegrationWorkers {
	test.Helper()
	owner, err := NewGitIntegrationWorkers(GitIntegrationWorkerConfig{Profile: GitIntegrationComplete, Metadata: metadata, Blobs: blobs, Repositories: repositories, Reader: reader, Authorizer: source, RequireCompleteMemory: true})
	if err != nil {
		test.Fatal(err)
	}
	return owner
}

type wave43PGScanStore struct {
	*store.PostgresStore
	test     *testing.T
	claims   atomic.Int64
	claimed  []domain.GitScanJob
	finished atomic.Int64
}

func (storage *wave43PGScanStore) ClaimGitScan(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.GitScanJob, error) {
	_, system := inbound.RepositoryActor(ctx)
	if !system || !reflect.DeepEqual(inbound.DocumentIdentities(ctx), ReadDocumentIdentities(storage, storage)) || lease != 2*time.Minute || storage.InTransaction(ctx) || ctx.Value(resourcePermitKey{}) == nil {
		storage.test.Error("direct scan claim lost binary support, actor, lease or admission boundary")
		return domain.GitScanJob{}, domain.ErrIntegrity
	}
	storage.claims.Add(1)
	job, err := storage.PostgresStore.ClaimGitScan(ctx, repo, now, lease)
	if err == nil {
		storage.claimed = append(storage.claimed, job)
	}
	return job, err
}

func (storage *wave43PGScanStore) FinishGitScan(ctx context.Context, finish domain.GitScanFinish) error {
	if !storage.InTransaction(ctx) || storage.InReadOnlyTransaction(ctx) || ctx.Value(afterCommitKey{}) == nil {
		storage.test.Error("scan publication escaped the original write transaction")
		return domain.ErrIntegrity
	}
	storage.finished.Add(1)
	return storage.PostgresStore.FinishGitScan(ctx, finish)
}

type wave43PGScanReader struct {
	*scanReader
	storage   *store.PostgresStore
	repo      domain.ContentHash
	treeReads atomic.Int64
	afterTree func(context.Context) error
}

func (reader *wave43PGScanReader) ReadCommitTree(ctx context.Context, origin, commit string) (domain.GitTreeEvidence, error) {
	deadline, bounded := ctx.Deadline()
	if reader.storage.InTransaction(ctx) || !bounded || time.Until(deadline) > 90*time.Second || outbound.GitRepository(ctx) != reader.repo {
		return domain.GitTreeEvidence{}, domain.ErrIntegrity
	}
	reader.treeReads.Add(1)
	evidence, err := reader.scanReader.ReadCommitTree(ctx, origin, commit)
	if err == nil && reader.afterTree != nil {
		err = reader.afterTree(ctx)
	}
	return evidence, err
}

func wave43PGScanFixture(test *testing.T) (*GitHubConnections, *wave43PGScanStore, *wave43PGScanReader, domain.Repo, *pgx.Conn) {
	test.Helper()
	wave40IsolatedPG(test)
	source, postgres, repo := providerObservationPGFixtureAt(test, os.Getenv("CXT_TEST_DSN"))
	storage := &wave43PGScanStore{PostgresStore: postgres, test: test}
	commit := strings.Repeat("a", 40)
	reader := &wave43PGScanReader{scanReader: &scanReader{deltas: map[string]domain.GitCommitDelta{commit: {Commit: commit, Parents: []string{}, Changes: []domain.GitPathChange{}, Complete: true}}}, storage: postgres, repo: repo.ID}
	if err := storage.EnqueueGitScan(wave40PGContext(context.Background(), postgres), domain.NewGitScan(repo.ID, repo.GitRemoteURL, commit, time.Now().UTC())); err != nil {
		test.Fatal(err)
	}
	return source, storage, reader, repo, wave43PGObserver(test, os.Getenv("CXT_TEST_DSN"))
}

func TestPGWave43GitIntegrationDirectScanSharedHeavyGateSQLLockCASAndRestart(test *testing.T) {
	source, storage, reader, repo, observer := wave43PGScanFixture(test)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	owner := wave43PGOwner(test, storage, storage, storage, reader, source)
	entered, release := make(chan struct{}), make(chan struct{})
	reader.afterTree = func(work context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-work.Done():
			return work.Err()
		}
	}
	finished, joined := make(chan error, 1), make(chan struct{})
	spoofed := inbound.WithDocumentIdentities(inbound.WithRepositoryActor(ctx, "caller"), []domain.DocumentIdentity{"caller-spoof"})
	go func() { defer close(joined); finished <- owner.Process(spoofed, 1) }()
	defer func() { cancel(); <-joined }()
	select {
	case <-entered:
	case <-ctx.Done():
		test.Fatal("direct owner never reached provider", ctx.Err())
	}
	if err := owner.Process(ctx, 1); !errors.Is(err, domain.ErrResourceOverloaded) || storage.claims.Load() != 1 {
		test.Fatal("concurrent Process bypassed the unchanged global heavy-one gate", storage.claims.Load(), err)
	}
	blocker, err := observer.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer wave42PGRollback(blocker)
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), 1735289201)`, string(repo.ID)); err != nil {
		test.Fatal(err)
	}
	close(release)
	if err := wave42PGWaitSQLBlock(ctx, blocker, "pg_advisory_xact_lock(hashtext"); err != nil {
		test.Fatal("publication skipped its actual repository lock", err)
	}
	if storage.finished.Load() != 0 {
		test.Fatal("scan finished before the SQL publication lock")
	}
	if err := blocker.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if err := <-finished; err != nil {
		test.Fatal(err)
	}
	<-joined
	reader.afterTree = nil
	for range 2 {
		owner = wave43PGOwner(test, storage, storage, storage, reader, source)
		if err := owner.Process(ctx, 1); err != nil {
			test.Fatal(err)
		}
	}
	claim := storage.claimed[0]
	done, err := storage.GetGitScan(wave40PGContext(ctx, storage.PostgresStore), repo.ID, claim.ID)
	if err != nil || done.State != "completed" || !done.TreeIndexed || !done.Indexed || done.Version != 3 || reader.treeReads.Load() != 1 || reader.reads != 1 {
		test.Fatal("durable scan phases changed across direct-owner reconstruction", done, err)
	}
	if err := owner.run(wave40PGContext(ctx, storage.PostgresStore), claim); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("expired direct execution replaced a newer scan version", err)
	}
	if current, err := storage.GetGitScan(wave40PGContext(ctx, storage.PostgresStore), repo.ID, claim.ID); err != nil || !reflect.DeepEqual(current, done) {
		test.Fatal("stale success or cleanup changed the terminal durable row", current, err)
	}
	reopened, err := store.NewPostgresStore(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	defer reopened.Close()
	restarted := wave43PGOwner(test, reopened, reopened, reopened, reader, source)
	if err := restarted.Process(ctx, 2); err != nil {
		test.Fatal(err)
	}
	if current, err := reopened.GetGitScan(wave40PGContext(ctx, reopened), repo.ID, claim.ID); err != nil || !reflect.DeepEqual(current, done) {
		test.Fatal("new adapter reclaimed completed immutable work", current, err)
	}
	for _, table := range []string{"refs", "context_history"} {
		var count int
		if err := observer.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE repo_id=$1", repo.ID).Scan(&count); err != nil || count != 0 {
			test.Fatal("scan moved context", table, count, err)
		}
	}
}

func TestPGWave43GitIntegrationDirectScanCurrentOriginAndSourceFences(test *testing.T) {
	for _, mutation := range []string{"origin", "source-generation", "source-revoked"} {
		test.Run(mutation, func(test *testing.T) {
			source, storage, reader, repo, observer := wave43PGScanFixture(test)
			ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), storage.PostgresStore), 15*time.Second)
			defer cancel()
			claim, err := storage.PostgresStore.ClaimGitScan(ctx, repo.ID, time.Now().UTC(), 2*time.Minute)
			if err != nil {
				test.Fatal(err)
			}
			reader.afterTree = func(work context.Context) error {
				if mutation == "origin" {
					changed, err := observer.Exec(work, `UPDATE repos SET git_remote_url=$2 WHERE id=$1`, repo.ID, "https://github.com/acme/rebound.git")
					if err == nil && changed.RowsAffected() != 1 {
						return domain.ErrIntegrity
					}
					return err
				}
				return storage.WithinIdentity(work, func(write context.Context) error {
					local, err := storage.GetRepository(write, repo.RepositoryID)
					if err != nil {
						return err
					}
					connection, err := storage.GetGitHubConnection(write, local.OwnerNamespaceID)
					if err != nil {
						return err
					}
					if mutation == "source-generation" {
						connection.Generation++
					} else {
						connection.Enabled = false
					}
					return storage.PutGitHubConnection(write, connection)
				})
			}
			owner := wave43PGOwner(test, storage, storage, storage, reader, source)
			err = owner.run(ctx, claim)
			want, state, reason := error(domain.ErrForbidden), "retrying", "temporary_provider_or_storage_failure"
			if mutation == "origin" {
				want, state, reason = domain.ErrConflict, "attention", "origin_or_lease_changed"
			}
			current, readErr := storage.GetGitScan(ctx, repo.ID, claim.ID)
			if !errors.Is(err, want) || readErr != nil || current.State != state || current.Reason != reason || current.TreeIndexed || current.Indexed || current.Version != claim.Version || !current.LeaseUntil.IsZero() {
				test.Fatal("post-provider current-policy fence failed", current, err, readErr)
			}
			if _, err := storage.GetGitCommitTree(ctx, repo.ID, repo.GitRemoteURL, claim.Commit); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("failed fence published a tree", err)
			}
		})
	}
}

func wave43ContributionFixture(test *testing.T) (*gitContributionApplyPGFixture, *contributionWorkerPGStore, *GitIntegrationWorkers) {
	test.Helper()
	wave40IsolatedPG(test)
	scans, basis, original, reader, _ := admissionBatchReconcileFixture(test, 1)
	metadata := &gitContributionApplyPGStore{admissionBatchPG: original}
	basis.worker.core.meta = metadata
	ctx := systemTestContext()
	base := basis.frozen.destination.Target
	capture := collaborationSnapshot(test, metadata.PostgresStore, basis.repo.ID, "wave43 independent pinned original", base)
	source := basis.frozen.source
	source.Target = capture
	if _, err := basis.worker.core.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: basis.repo.ID, Ref: source, Force: true}); err != nil {
		test.Fatal(err)
	}
	ordinary := domain.HistoryEvent{ID: strings.Repeat("2", 32), RepoID: string(basis.repo.ID), Branch: source.Name, BranchID: source.BranchID, Kind: "position", Source: capture, Target: capture, GitAfter: strings.Repeat("3", 40), WorktreeID: strings.Repeat("5", 32), CreatedAt: time.Unix(150, 0).UTC()}
	if err := basis.worker.core.RecordHistory(ctx, ordinary); err != nil {
		test.Fatal(err)
	}
	fixture := &gitContributionApplyPGFixture{scans: scans, basis: basis, storage: metadata, reader: reader, publication: prPublication(ordinary)}
	fixture.publication.MemoryHash, fixture.publication.MemorySource = fixture.memory(test, "original immutable pin"), capture
	if err := basis.worker.core.RecordHistory(ctx, fixture.publication); err != nil {
		test.Fatal(err)
	}
	metadata.trees[ordinary.GitAfter] = domain.GitCommitTree{Commit: ordinary.GitAfter, Tree: strings.Repeat("f", 40), Parents: []string{strings.Repeat("a", 40)}}
	storage := &contributionWorkerPGStore{gitContributionApplyPGStore: metadata, clock: time.Now().UTC()}
	owner := wave43PGOwner(test, storage, storage, basis.storage, reader, basis.worker)
	reader.headPages[1] = []outbound.GitHead{{Ref: "refs/heads/feature", Commit: ordinary.GitAfter}, {Ref: "refs/heads/main", Commit: strings.Repeat("b", 40)}}
	if err := owner.Reconcile(ctx); err != nil {
		test.Fatal(err)
	}
	admissions, err := storage.ListGitContributionAdmissions(ctx, basis.repo.ID, basis.repo.GitRemoteURL, "", 101)
	if err != nil || len(admissions) != 1 || admissions[0].PublicationID != fixture.publication.ID {
		test.Fatal("direct reconciler lost the independent original publication", admissions, err)
	}
	fixture.admission = admissions[0]
	return fixture, storage, owner
}

func wave43BoundIntegration(test *testing.T, fixture *gitContributionApplyPGFixture, owner *GitIntegrationWorkers) preparedGitContribution {
	test.Helper()
	fixture.indexIntegration()
	fixture.reader.headPages[1] = []outbound.GitHead{{Ref: "refs/heads/main", Commit: strings.Repeat("d", 40)}}
	ctx := systemTestContext()
	if err := owner.Reconcile(ctx); err != nil {
		test.Fatal(err)
	}
	heads, err := fixture.storage.ListGitContextHeads(ctx, fixture.basis.repo.ID, fixture.basis.repo.GitRemoteURL, fixture.admission.Destination.DestinationBranchID, fixture.admission.AfterReadVersion, 101)
	if err != nil || len(heads) != 1 {
		test.Fatal("direct reconciliation failed to persist its bound occurrence", heads, err)
	}
	fixture.head = heads[0]
	prepared, err := owner.prepareGitContribution(ctx, fixture.admission, fixture.head)
	if err != nil || prepared.snapshot == nil || prepared.snapshot.generation == nil || prepared.memory == nil {
		test.Fatal("direct preparation lost committed generation or original pin", err)
	}
	return prepared
}

func TestPGWave43GitIntegrationDirectContributionAtomicPinnedRewindAndRestart(test *testing.T) {
	fixture, storage, owner := wave43ContributionFixture(test)
	prepared := wave43BoundIntegration(test, fixture, owner)
	ctx := systemTestContext()
	mutable := fixture.memory(test, "unpublished replacement must stay invisible")
	if err := storage.CompareAndSwapSnapshotMemory(ctx, fixture.basis.repo.ID, fixture.admission.Capture, "", mutable); err != nil {
		test.Fatal(err)
	}
	fixture.reader.headPages[1] = []outbound.GitHead{{Ref: "refs/heads/main", Commit: strings.Repeat("b", 40)}}
	if err := owner.Reconcile(ctx); err != nil {
		test.Fatal(err)
	}
	before := fixture.state(test, ctx)
	storage.rejectFinish = true
	if err := owner.ProcessContributions(ctx, 1); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("failed completion was not propagated", err)
	}
	if after := fixture.state(test, ctx); !reflect.DeepEqual(before, after) {
		test.Fatal("completion failure leaked proof, pin, revision or context", after)
	}
	storage.rejectFinish = false
	owner = wave43PGOwner(test, storage, storage, fixture.basis.storage, fixture.reader, fixture.basis.worker)
	providerReads := len(fixture.reader.headReads)
	if err := owner.ProcessContributions(ctx, 1); err != nil {
		test.Fatal(err)
	}
	after := fixture.state(test, ctx)
	assertGitContributionAppliedOnce(test, before, after, prepared)
	if after.memories[0].MemoryHash != fixture.publication.MemoryHash || after.memories[0].MemoryHash == mutable || after.proofs[0].ContextHeadID != fixture.head.ID || len(fixture.reader.headReads) != providerReads {
		test.Fatal("rewind selected mutable memory, a new provider read or an unbound occurrence", after)
	}
	owner = wave43PGOwner(test, storage, storage, fixture.basis.storage, fixture.reader, fixture.basis.worker)
	if err := owner.ProcessContributions(ctx, 1); err != nil || !reflect.DeepEqual(after, fixture.state(test, ctx)) {
		test.Fatal("restarted contribution duplicated a durable effect", err)
	}
}

func TestPGWave43GitIntegrationDirectFrozenGenerationRejectsHiddenHistoryMutation(test *testing.T) {
	fixture, storage, owner := wave43ContributionFixture(test)
	prepared := wave43BoundIntegration(test, fixture, owner)
	ctx := systemTestContext()
	before, err := storage.RepositoryRevision(ctx, fixture.basis.repo.ID)
	if err != nil {
		test.Fatal(err)
	}
	competing := domain.HistoryEvent{ID: strings.Repeat("6", 32), RepoID: string(fixture.basis.repo.ID), Kind: "position", Branch: "other-source", BranchID: "other-incarnation", Source: fixture.admission.Capture, Target: fixture.admission.Capture, GitAfter: fixture.admission.SourceHead, CreatedAt: time.Now().UTC()}
	if err := storage.ApplyHistoryEvent(ctx, competing); err != nil {
		test.Fatal(err)
	}
	if after, err := storage.RepositoryRevision(ctx, fixture.basis.repo.ID); err != nil || after != before {
		test.Fatal("fixture must bypass application revision", after, before, err)
	}
	state := fixture.state(test, ctx)
	authorization, err := authorizeGitAdmission(ctx, fixture.basis.worker, fixture.basis.repo.ID)
	if err != nil {
		test.Fatal(err)
	}
	if proof, err := owner.applyGitContribution(ctx, prepared, authorization); !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(proof, domain.GitContributionProof{}) {
		test.Fatal("stale direct proof crossed the committed-generation fence", proof, err)
	}
	if !reflect.DeepEqual(state, fixture.state(test, ctx)) {
		test.Fatal("rejected frozen generation changed durable effects")
	}
	if _, err := owner.prepareGitContribution(ctx, fixture.admission, fixture.head); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("fresh direct preparation ignored a competing source owner", err)
	}
}

func TestPGWave43GitIntegrationDirectLatePRMemorySourceAndSupplementReplay(test *testing.T) {
	wave40IsolatedPG(test)
	fixture, storage := newSupplementPGFixture(test)
	ctx := systemTestContext()
	publication := fixture.publication
	publication.ID = strings.Repeat("f", 32)
	digest := domain.MemoryDigest{SnapshotID: publication.Target, PreviousMemoryHash: publication.MemoryHash, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: publication.Target, Claims: []domain.MemoryClaim{{Kind: "code", Text: "revised published memory", Code: &domain.MemoryCodeScope{Commit: publication.GitAfter, Paths: []string{"file"}}}}}}}
	hash, err := storage.PutMemory(ctx, fixture.basis.repo.ID, digest)
	if err != nil {
		test.Fatal(err)
	}
	publication.MemoryHash = hash
	if err := fixture.basis.worker.core.RecordHistory(ctx, publication); err != nil {
		test.Fatal(err)
	}
	owner := wave43PGOwner(test, storage, storage, fixture.basis.storage, fixture.reader, fixture.basis.worker)
	if _, err := owner.discoverGitAssociations(ctx, fixture.basis.repo); err != nil {
		test.Fatal(err)
	}
	before := fixture.state(test, ctx)
	storage.missingMemory = publication.MemoryHash
	if err := owner.ProcessContributions(ctx, 1); err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(before, fixture.state(test, ctx)) {
		test.Fatal("missing exact publication memory fell back to a mutable pointer")
	}
	storage.missingMemory = ""
	owner = wave43PGOwner(test, storage, storage, fixture.basis.storage, fixture.reader, fixture.basis.worker)
	if err := owner.ProcessContributions(ctx, 1); err != nil {
		test.Fatal(err)
	}
	assertGitSupplementEffect(test, fixture, before, publication)
	after := fixture.state(test, ctx)
	if err := owner.ProcessContributions(ctx, 1); err != nil || !reflect.DeepEqual(after, fixture.state(test, ctx)) {
		test.Fatal("restarted supplement repeated an effect", err)
	}
}
