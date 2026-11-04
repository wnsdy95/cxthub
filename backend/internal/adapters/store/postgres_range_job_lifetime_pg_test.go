//go:build postgres

package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type rangeJobLifetimeFixturePG struct {
	scope    rootRepoCascadeFixturePG
	source   legacyByteStagePGFixture
	snapshot domain.Snapshot
	claim    domain.DocRangeIndexJob
}

func newRangeJobLifetimeFixturePG(test *testing.T) rangeJobLifetimeFixturePG {
	test.Helper()
	database, storage := authorityScopePG(test)
	directory := rangeSpoolTempDir(test)
	test.Cleanup(func() { assertRangeSpoolsRemoved(test, directory) })
	var queued int
	if err := storage.pool.QueryRow(database.ctx, `SELECT count(*) FROM doc_range_jobs_v1`).Scan(&queued); err != nil || queued != 0 {
		test.Fatal("actual ClaimDocRangeJob requires its own fresh database, not the shared global queue", queued, err)
	}
	storage.ConfigureLegacyByteRangePublication(true)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	source := legacyByteStagePGSource(test, storage, database.ctx, 8<<10)
	repo, err := storage.GetRepo(database.ctx, source.repo)
	if err != nil {
		test.Fatal(err)
	}
	repository, err := storage.GetRepository(database.ctx, repo.RepositoryID)
	if err != nil {
		test.Fatal(err)
	}
	survivor, survivorNamespace := docRangeJobPGRepo(test, storage, database.ctx)
	fixture := rangeJobLifetimeFixturePG{
		scope: rootRepoCascadeFixturePG{
			storage: storage, ctx: database.ctx, repo: repo, repository: repository,
			namespace: repository.OwnerNamespaceID, survivor: survivor, survivorNamespace: survivorNamespace,
		},
		source: source,
	}
	if _, err := storage.pool.Exec(database.ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, survivor, source.hash); err != nil {
		test.Fatal("seed the second owner's grant to the existing monolithic fixture without converting its representation", err)
	}
	fixture.scope.sources = map[domain.ContentHash]rootSourceWitnessStatePG{
		source.hash: readRootSourceWitnessStatePG(test, storage, database.ctx, source.hash),
	}
	fixture.snapshot, err = storage.GetSnapshot(database.ctx, source.repo, source.hash)
	if err != nil || fixture.snapshot.DocIdentity != domain.DocumentIdentityLegacy || fixture.snapshot.DocHash != source.hash {
		test.Fatal("retain the original typed snapshot inputs", fixture.snapshot, err)
	}
	fixture.scope.epoch = rootRepoCascadeEpochPG(test, fixture.scope, source.repo)
	fixture.scope.survivorEpoch = rootRepoCascadeEpochPG(test, fixture.scope, survivor)
	fixture.scope.survivorImage = rootPGImage(test, database.ctx, storage, survivor, survivorNamespace)
	waiting, err := storage.EnqueueDocByteRangeJob(database.ctx, source.repo, source.hash, time.Now().UTC().Add(-time.Hour))
	if err != nil || waiting.State != "waiting" || waiting.Version != 0 || waiting.Attempts != 0 || !waiting.LeaseUntil.IsZero() {
		test.Fatal("enqueue original waiting version-zero byte-range job", waiting, err)
	}
	fixture.claim = legacyByteStagePGClaim(test, storage, database.ctx, source)
	if fixture.claim.Version != 1 || fixture.claim.Attempts != 1 || !fixture.claim.CreatedAt.Equal(waiting.CreatedAt) {
		test.Fatal("actual original claim must start at version one", fixture.claim, waiting)
	}
	assertRootRepoCascadeSurvivorPG(test, fixture.scope)
	return fixture
}

func recreateRangeJobLifetimePG(test *testing.T, fixture rangeJobLifetimeFixturePG) (domain.DocRangeIndexJob, string) {
	test.Helper()
	scope := fixture.scope
	storage, ctx := scope.storage, scope.ctx
	test.Log("privileged owned-fixture transaction removes snapshots then repos under existing foreign keys; this is not a public repository deletion API")
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if deleted, err := transaction.Exec(ctx, `DELETE FROM snapshots WHERE repo_id=$1`, scope.repo.ID); err != nil || deleted.RowsAffected() != 1 {
		test.Fatal("remove the owned snapshot before deleting its referenced repo", deleted.RowsAffected(), err)
	}
	deleted, err := transaction.Exec(ctx, `DELETE FROM repos WHERE id=$1`, scope.repo.ID)
	if err != nil || deleted.RowsAffected() != 1 {
		test.Fatal("delete exactly the owned repos row using actual PostgreSQL cascades", deleted.RowsAffected(), err)
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.GetRepo(ctx, scope.repo.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("actual cascade retained the original repos row", err)
	}
	if _, err := storage.GetSnapshot(ctx, scope.repo.ID, fixture.snapshot.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("actual cascade retained the original typed snapshot", err)
	}
	var remaining [4]int
	if err := storage.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1),
 (SELECT count(*) FROM repo_blobs WHERE repo_id=$1),
 (SELECT count(*) FROM snapshots WHERE repo_id=$1),
 (SELECT count(*) FROM repository_catalog_state WHERE repo_id=$1)`, scope.repo.ID).
		Scan(&remaining[0], &remaining[1], &remaining[2], &remaining[3]); err != nil || remaining != [4]int{} {
		test.Fatal("actual cascade did not remove range receipts, grants, snapshots, and lifetime state", remaining, err)
	}
	assertRootRepoCascadeSurvivorPG(test, scope)
	recreated, err := storage.PutRepo(ctx, scope.repo)
	if err != nil || !reflect.DeepEqual(recreated, scope.repo) {
		test.Fatal("PutRepo must recreate the identical repo identity and containing repository metadata", recreated, scope.repo, err)
	}
	epoch := rootRepoCascadeEpochPG(test, scope, scope.repo.ID)
	if epoch == scope.epoch {
		test.Fatal("actual repos cascade and PutRepo reused the old catalog lifetime UUID", epoch)
	}
	if _, err := storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, scope.repo.ID, fixture.source.hash); err != nil {
		test.Fatal("restore only the owned fixture grant without replacing the surviving physical source", err)
	}
	if err := storage.PutSnapshot(ctx, fixture.snapshot); err != nil {
		test.Fatal("restore the original typed snapshot inputs", err)
	}
	actualSnapshot, err := storage.GetSnapshot(ctx, scope.repo.ID, fixture.snapshot.ID)
	actualSnapshot.CreatedAt = fixture.snapshot.CreatedAt
	if err != nil || !reflect.DeepEqual(actualSnapshot, fixture.snapshot) {
		test.Fatal("restored snapshot inputs changed beyond the database-generated creation time", actualSnapshot, fixture.snapshot, err)
	}
	waiting, err := storage.EnqueueDocByteRangeJob(ctx, scope.repo.ID, fixture.source.hash, fixture.claim.CreatedAt.Add(30*time.Minute))
	if err != nil || waiting.RepoID != fixture.claim.RepoID || waiting.DocHash != fixture.claim.DocHash || waiting.BaseHash != fixture.claim.BaseHash ||
		waiting.State != "waiting" || waiting.Version != 0 || waiting.Attempts != 0 || !waiting.LeaseUntil.IsZero() || !waiting.CreatedAt.After(fixture.claim.CreatedAt) {
		test.Fatal("reenqueue the same job identity as a new waiting version-zero receipt", waiting, fixture.claim, err)
	}
	current := docRangeJobPGClaim(test, storage, ctx, scope.repo.ID, fixture.source.hash)
	if current.RepoID != fixture.claim.RepoID || current.DocHash != fixture.claim.DocHash || current.BaseHash != fixture.claim.BaseHash ||
		current.Version != 1 || current.Attempts != 1 || !current.CreatedAt.Equal(waiting.CreatedAt) || current.LeaseUntil.Equal(fixture.claim.LeaseUntil) {
		test.Fatal("actual claim must reproduce the identical repo/doc/base/version-one ABA with a distinct lease and creation time", current, fixture.claim)
	}
	if !fixture.claim.LeaseUntil.After(time.Now()) || !current.LeaseUntil.After(time.Now()) {
		test.Fatal("both original and replacement claims must remain unexpired for the lifetime-fence test")
	}
	assertRootRepoCascadeSurvivorPG(test, scope)
	test.Logf("same range job (%s,%s,%s), version 1; repository lifetime %s -> %s; physical source incarnation/version unchanged",
		current.RepoID, current.DocHash, current.BaseHash, scope.epoch, epoch)
	return current, epoch
}

func rangeJobLifetimeImagePG(test *testing.T, fixture rangeJobLifetimeFixturePG) [5]string {
	test.Helper()
	scope := fixture.scope
	var authority string
	if err := scope.storage.pool.QueryRow(scope.ctx, `SELECT jsonb_build_object(
 'repos',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(repo),repo.xmin::text) ORDER BY repo.id)
 FROM repos repo WHERE repo.id IN ($1,$2)),
 'catalog',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(catalog),catalog.xmin::text) ORDER BY catalog.repo_id)
 FROM repository_catalog_state catalog WHERE catalog.repo_id IN ($1,$2)),
 'metadata',(SELECT jsonb_build_array(to_jsonb(repository),repository.xmin::text) FROM repositories repository WHERE repository.id=$3)
 )::text`, scope.repo.ID, scope.survivor, scope.repository.ID).Scan(&authority); err != nil {
		test.Fatal("capture both repository lifetime and metadata witnesses", err)
	}
	return [5]string{
		authority,
		rootPGImage(test, scope.ctx, scope.storage, scope.repo.ID, scope.namespace),
		rootPGImage(test, scope.ctx, scope.storage, scope.survivor, scope.survivorNamespace),
		legacyByteStagePGPublicImage(test, scope.storage, scope.ctx, fixture.source),
		legacyByteRecoveryFencePGImage(test, scope.storage, scope.ctx, fixture.source.hash),
	}
}

func assertRangeJobLifetimeFreshCompletionPG(test *testing.T, fixture rangeJobLifetimeFixturePG, current domain.DocRangeIndexJob, epoch string) {
	test.Helper()
	scope := fixture.scope
	if err := scope.storage.RenewDocRangeJob(scope.ctx, current, time.Now().UTC(), 3*time.Minute); err != nil {
		test.Fatal("the current lifetime's actual version-one claim must still renew", err)
	}
	renewed := docRangeJobPGGet(test, scope.storage, scope.ctx, current.RepoID, current.DocHash)
	expected := current
	expected.RepositoryEpoch = ""
	expected.LeaseUntil = renewed.LeaseUntil
	if !renewed.LeaseUntil.After(current.LeaseUntil) || !reflect.DeepEqual(renewed, expected) {
		test.Fatal("fresh renewal must only extend the current lease", renewed, current)
	}
	current.LeaseUntil = renewed.LeaseUntil
	if err := scope.storage.BuildDocRangeJob(scope.ctx, current); err != nil {
		test.Fatal("the current lifetime's real byte-range work must still publish and complete", err)
	}
	completed := docRangeJobPGGet(test, scope.storage, scope.ctx, current.RepoID, current.DocHash)
	expected.State, expected.LeaseUntil = "completed", time.Time{}
	if !reflect.DeepEqual(completed, expected) {
		test.Fatal("fresh build lost or changed the version-one completion receipt", completed, expected)
	}
	legacyDerivedPublishedPG(test, scope.storage, scope.ctx, fixture.source)
	for hash, source := range scope.sources {
		assertRootSourceWitnessStatePG(test, scope.storage, scope.ctx, hash, source)
	}
	if actual := rootRepoCascadeEpochPG(test, scope, scope.repo.ID); actual != epoch {
		test.Fatal("fresh completion changed its current repository lifetime UUID", actual, epoch)
	}
}

func TestDocRangeJobLifetimePGRecreatedVersionOneRejectsOldClaim(test *testing.T) {
	for _, operation := range []string{"renew", "finish-retrying", "finish-rejected", "prepare", "build", "prepared-step"} {
		test.Run(operation, func(test *testing.T) {
			fixture := newRangeJobLifetimeFixturePG(test)
			scope := fixture.scope
			var prepared outbound.PreparedDocRangeWork
			if operation == "prepared-step" {
				before := rangeJobLifetimeImagePG(test, fixture)
				work, err := scope.storage.PrepareDocRangeWork(scope.ctx, fixture.claim)
				if work != nil {
					test.Cleanup(func() {
						if err := work.Close(); err != nil {
							test.Error("close work prepared before the actual repository cascade", err)
						}
					})
				}
				if err != nil || work == nil {
					test.Fatal("prepare real work while the original lifetime is still live", err)
				}
				prepared = work
				if after := rangeJobLifetimeImagePG(test, fixture); after != before {
					test.Fatal("original preparation changed durable state")
				}
			}
			current, epoch := recreateRangeJobLifetimePG(test, fixture)
			before := rangeJobLifetimeImagePG(test, fixture)
			var failure error
			switch operation {
			case "renew":
				failure = scope.storage.RenewDocRangeJob(scope.ctx, fixture.claim, time.Now().UTC(), 10*time.Minute)
			case "finish-retrying", "finish-rejected":
				stale := fixture.claim
				stale.State = "retrying"
				if operation == "finish-rejected" {
					stale.State = "rejected"
				}
				stale.Reason = "old repository lifetime must not consume the fresh receipt"
				stale.NextAttempt = time.Now().UTC().Add(time.Hour)
				stale.LeaseUntil = time.Time{}
				failure = scope.storage.FinishDocRangeJob(scope.ctx, stale, time.Now().UTC())
			case "prepare":
				work, err := scope.storage.PrepareDocRangeWork(scope.ctx, fixture.claim)
				failure = err
				if work != nil {
					test.Error("old repository lifetime received prepared work for the replacement claim")
					if err := work.Close(); err != nil {
						test.Error("close work returned to the stale lifetime", err)
					}
				}
			case "build":
				failure = scope.storage.BuildDocRangeJob(scope.ctx, fixture.claim)
			case "prepared-step":
				progress, err := prepared.Step(scope.ctx, fixture.claim)
				failure = err
				if progress != 0 {
					test.Errorf("old prepared work crossed the repository lifetime and advanced a source-reading step: progress=%v, error=%v", progress, err)
				}
			}
			test.Logf("%s with original claim returned: %v", operation, failure)
			if !errors.Is(failure, domain.ErrConflict) && !errors.Is(failure, domain.ErrNotFound) {
				test.Errorf("%s crossed repository lifetime %s -> %s at the same range job/version 1: got %v, want ErrConflict or ErrNotFound",
					operation, scope.epoch, epoch, failure)
			}
			expectedReceipt := current
			expectedReceipt.RepositoryEpoch = ""
			if after := docRangeJobPGGet(test, scope.storage, scope.ctx, current.RepoID, current.DocHash); !reflect.DeepEqual(after, expectedReceipt) {
				test.Errorf("old %s changed fresh running version-one lease or receipt: got %+v, want %+v", operation, after, current)
			}
			if after := rangeJobLifetimeImagePG(test, fixture); after != before {
				test.Errorf("old %s left durable effects in repository lifetime/metadata, ownership, accounting, source, indexes, byte staging, or receipt", operation)
			}
			assertRootRepoCascadeSurvivorPG(test, scope)
			if test.Failed() {
				return
			}
			assertRangeJobLifetimeFreshCompletionPG(test, fixture, current, epoch)
		})
	}
}

func TestDocRangeJobLifetimePGFreshClaimCompletesAfterRecreation(test *testing.T) {
	fixture := newRangeJobLifetimeFixturePG(test)
	current, epoch := recreateRangeJobLifetimePG(test, fixture)
	assertRangeJobLifetimeFreshCompletionPG(test, fixture, current, epoch)
}
