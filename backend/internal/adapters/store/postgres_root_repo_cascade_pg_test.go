//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type rootRepoCascadeFixturePG struct {
	storage           *PostgresStore
	ctx               context.Context
	repo              domain.Repo
	repository        domain.Repository
	namespace         string
	root              rootReaderFixture
	claim             domain.DocFinalizationJob
	epoch             string
	survivor          domain.ContentHash
	survivorNamespace string
	survivorImage     string
	survivorEpoch     string
	sources           map[domain.ContentHash]rootSourceWitnessStatePG
}

func rootRepoCascadeEpochPG(test *testing.T, fixture rootRepoCascadeFixturePG, repo domain.ContentHash) string {
	test.Helper()
	var epoch string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT epoch::text FROM repository_catalog_state WHERE repo_id=$1`, repo).Scan(&epoch); err != nil || epoch == "" {
		test.Fatal("read existing repository lifetime UUID", epoch, err)
	}
	return epoch
}

func newRootRepoCascadeFixturePG(test *testing.T, count int) rootRepoCascadeFixturePG {
	test.Helper()
	directory := rangeSpoolTempDir(test)
	test.Cleanup(func() { assertRangeSpoolsRemoved(test, directory) })
	storage, ctx := chunkReusePG(test)
	ctx = rootPublicationContext(ctx)
	repo, namespace := rootPGRepo(test, ctx, storage)
	metadata, err := storage.GetRepo(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	repository, err := storage.GetRepository(ctx, metadata.RepositoryID)
	if err != nil {
		test.Fatal(err)
	}
	survivor, survivorNamespace := rootPGRepo(test, ctx, storage)
	fixture := rootRepoCascadeFixturePG{
		storage: storage, ctx: ctx, repo: metadata, repository: repository, namespace: namespace,
		root: rangeFixture(test, count), survivor: survivor, survivorNamespace: survivorNamespace,
		sources: make(map[domain.ContentHash]rootSourceWitnessStatePG),
	}
	if count > 0 && len(fixture.root.bodies) == 0 || count == 0 && len(fixture.root.bodies) != 0 {
		test.Fatal("fixture does not isolate empty roots from shared dependency roots")
	}
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal(err)
	}
	if _, _, err := storage.PutChunks(ctx, repo, fixture.root.bodies); err != nil {
		test.Fatal(err)
	}
	if _, _, err := storage.PutChunks(ctx, survivor, fixture.root.bodies); err != nil {
		test.Fatal(err)
	}
	for hash := range fixture.root.bodies {
		fixture.sources[hash] = readRootSourceWitnessStatePG(test, storage, ctx, hash)
	}
	submission, err := domain.NewDocFinalizationJobForRepresentation(repo, domain.DocumentRepresentation{
		Hash: fixture.root.hash, Identity: domain.DocumentIdentityRootV1, RootManifest: fixture.root.manifestBytes,
	}, time.Now().UTC().Add(-time.Second))
	if err != nil {
		test.Fatal(err)
	}
	queued, err := storage.EnqueueDocJob(ctx, submission)
	if err != nil || queued.ID != submission.ID || queued.State != "waiting" || queued.Version != 0 {
		test.Fatal("enqueue original root through public store operation", queued, err)
	}
	fixture.claim, err = storage.ClaimDocJob(ctx, repo, time.Now().UTC(), time.Minute)
	if err != nil || fixture.claim.ID != submission.ID || fixture.claim.State != "running" || fixture.claim.Version != 1 || fixture.claim.Attempts != 1 {
		test.Fatal("claim original version-one root through public store operation", fixture.claim, err)
	}
	fixture.epoch = rootRepoCascadeEpochPG(test, fixture, repo)
	fixture.survivorEpoch = rootRepoCascadeEpochPG(test, fixture, survivor)
	fixture.survivorImage = rootPGImage(test, ctx, storage, survivor, survivorNamespace)
	assertRootRepoCascadeUnpublishedPG(test, fixture)
	return fixture
}

func prepareRootRepoCascadePG(test *testing.T, fixture rootRepoCascadeFixturePG, claim domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	test.Helper()
	publication, err := fixture.storage.PrepareRootDocJob(fixture.ctx, claim)
	if publication != nil {
		test.Cleanup(func() {
			if closeErr := publication.Close(); closeErr != nil {
				test.Error("close repository cascade publication", closeErr)
			}
		})
	}
	if err != nil && publication != nil || err == nil && publication == nil {
		test.Fatal("preparation returned inconsistent publication and error", err)
	}
	return publication, err
}

func assertRootRepoCascadeUnpublishedPG(test *testing.T, fixture rootRepoCascadeFixturePG) {
	test.Helper()
	var counts [4]int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT
 (SELECT count(*) FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2),
 (SELECT count(*) FROM blobs WHERE hash=$2),
 (SELECT count(*) FROM blob_source_incarnations WHERE hash=$2),
 (SELECT count(*) FROM doc_read_indexes_v3 WHERE hash=$2)`, fixture.repo.ID, fixture.root.hash).
		Scan(&counts[0], &counts[1], &counts[2], &counts[3]); err != nil {
		test.Fatal(err)
	}
	if counts != [4]int{} {
		test.Error("stale publication created root grant, body, source incarnation, or legacy index", counts)
	}
	if ranges := rangeCountsPG(test, fixture.storage, fixture.ctx, fixture.root.hash); ranges != [2]int{} {
		test.Error("stale publication created range header or pages", ranges)
	}
}

func assertRootRepoCascadeSurvivorPG(test *testing.T, fixture rootRepoCascadeFixturePG) {
	test.Helper()
	if _, err := fixture.storage.GetRepo(fixture.ctx, fixture.survivor); err != nil {
		test.Fatal("repository cascade removed the other owner", err)
	}
	if image := rootPGImage(test, fixture.ctx, fixture.storage, fixture.survivor, fixture.survivorNamespace); image != fixture.survivorImage {
		test.Error("repository cascade or stale completion changed the other owner's grants, raw bytes, accounting, indexes, or jobs")
	}
	if epoch := rootRepoCascadeEpochPG(test, fixture, fixture.survivor); epoch != fixture.survivorEpoch {
		test.Error("repository cascade changed the other owner's lifetime UUID", epoch, fixture.survivorEpoch)
	}
	for hash, source := range fixture.sources {
		assertRootSourceWitnessStatePG(test, fixture.storage, fixture.ctx, hash, source)
	}
	repository, err := fixture.storage.GetRepository(fixture.ctx, fixture.repository.ID)
	if err != nil || !reflect.DeepEqual(repository, fixture.repository) {
		test.Error("privileged repos-row deletion changed the original containing repository metadata", repository, err)
	}
}

func assertRootRepoCascadeDeletedPG(test *testing.T, fixture rootRepoCascadeFixturePG) {
	test.Helper()
	if _, err := fixture.storage.GetRepo(fixture.ctx, fixture.repo.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Error("deleted repos row became visible", err)
	}
	if _, err := fixture.storage.GetDocJob(fixture.ctx, fixture.repo.ID, fixture.claim.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Error("cascaded root job became visible", err)
	}
	var remaining [3]int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT
 (SELECT count(*) FROM doc_finalization_jobs WHERE repo_id=$1),
 (SELECT count(*) FROM repo_blobs WHERE repo_id=$1),
 (SELECT count(*) FROM repository_catalog_state WHERE repo_id=$1)`, fixture.repo.ID).
		Scan(&remaining[0], &remaining[1], &remaining[2]); err != nil {
		test.Fatal(err)
	}
	if remaining != [3]int{} {
		test.Error("actual repository cascade retained or resurrected jobs, grants, or lifetime state", remaining)
	}
	assertRootRepoCascadeUnpublishedPG(test, fixture)
	assertRootRepoCascadeSurvivorPG(test, fixture)
}

func deleteRootRepoCascadeOwnedFixturePG(test *testing.T, fixture rootRepoCascadeFixturePG) {
	test.Helper()
	test.Log("privileged owned-fixture DELETE FROM repos; this does not exercise a public repository deletion API")
	deleted, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM repos WHERE id=$1`, fixture.repo.ID)
	if err != nil || deleted.RowsAffected() != 1 {
		test.Fatal("delete exactly the owned repos row using actual schema cascades", deleted.RowsAffected(), err)
	}
	assertRootRepoCascadeDeletedPG(test, fixture)
	if test.Failed() {
		test.FailNow()
	}
}

func TestRootRepoCascadePGRecreatedVersionOneRejectsStalePublication(test *testing.T) {
	for _, root := range []struct {
		name  string
		count int
	}{{"empty", 0}, {"shared-dependencies", domain.DocRangePageEvents + 1}} {
		for _, phase := range []string{"old-prepared-old-claim", "old-claim-prepared-after-recreation", "old-prepared-current-claim"} {
			test.Run(root.name+"/"+phase, func(test *testing.T) {
				fixture := newRootRepoCascadeFixturePG(test, root.count)
				var stale outbound.PreparedRootDocPublication
				var err error
				if phase != "old-claim-prepared-after-recreation" {
					before := rootPGImage(test, fixture.ctx, fixture.storage, fixture.repo.ID, fixture.namespace)
					stale, err = prepareRootRepoCascadePG(test, fixture, fixture.claim)
					if err != nil {
						test.Fatal("prepare original claimed root before repository cascade", err)
					}
					if after := rootPGImage(test, fixture.ctx, fixture.storage, fixture.repo.ID, fixture.namespace); after != before {
						test.Fatal("preparation changed durable repository state")
					}
				}
				deleteRootRepoCascadeOwnedFixturePG(test, fixture)
				recreated, err := fixture.storage.PutRepo(fixture.ctx, fixture.repo)
				if err != nil || !reflect.DeepEqual(recreated, fixture.repo) {
					test.Fatal("recreate same repos row under original repository metadata through PutRepo", recreated, err)
				}
				epoch := rootRepoCascadeEpochPG(test, fixture, fixture.repo.ID)
				if epoch == fixture.epoch {
					test.Fatal("actual repository cascade/recreation reused its catalog lifetime UUID", epoch)
				}
				if err := fixture.storage.RequireDocumentIdentity(fixture.ctx, fixture.repo.ID, domain.DocumentIdentityRootV1); err != nil {
					test.Fatal(err)
				}
				if _, _, err := fixture.storage.PutChunks(fixture.ctx, fixture.repo.ID, fixture.root.bodies); err != nil {
					test.Fatal("restore dependency grants through public PutChunks without replacing shared blobs", err)
				}
				submission := rootPublicationJob(test, fixture.repo.ID, fixture.root)
				queued, err := fixture.storage.EnqueueDocJob(fixture.ctx, submission)
				if err != nil || queued.ID != fixture.claim.ID || queued.State != "waiting" || queued.Version != 0 || !queued.CreatedAt.After(fixture.claim.CreatedAt) {
					test.Fatal("public reenqueue did not recreate deterministic job ID at version zero with a fresh creation time", queued, err)
				}
				current, err := fixture.storage.ClaimDocJob(fixture.ctx, fixture.repo.ID, time.Now().UTC(), time.Minute)
				if err != nil || current.ID != fixture.claim.ID || current.RepoID != fixture.claim.RepoID || current.DocumentRef() != fixture.claim.DocumentRef() || current.Version != 1 || current.Attempts != 1 || current.State != "running" || !current.CreatedAt.After(fixture.claim.CreatedAt) || current.LeaseUntil.Equal(fixture.claim.LeaseUntil) {
					test.Fatal("public claim did not reproduce same root/job ID/version-one ABA with a distinct lease", current, err)
				}
				assertRootRepoCascadeUnpublishedPG(test, fixture)
				assertRootRepoCascadeSurvivorPG(test, fixture)
				before := rootPGImage(test, fixture.ctx, fixture.storage, fixture.repo.ID, fixture.namespace)
				if err := fixture.storage.RenewDocJob(fixture.ctx, fixture.claim, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("an old repository lifetime renewed the replacement job lease", err)
				}
				late := fixture.claim
				late.State = "retrying"
				late.Reason = "stale lifetime must not finish a new job"
				late.LeaseUntil = time.Time{}
				if err := fixture.storage.FinishDocJob(fixture.ctx, late, time.Now().UTC()); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("an old repository lifetime transitioned the replacement job", err)
				}
				if after := rootPGImage(test, fixture.ctx, fixture.storage, fixture.repo.ID, fixture.namespace); after != before {
					test.Fatal("stale renew/finish changed the replacement job or ownership")
				}
				if phase == "old-claim-prepared-after-recreation" {
					stale, err = prepareRootRepoCascadePG(test, fixture, fixture.claim)
					if err != nil && !errors.Is(err, domain.ErrConflict) {
						test.Fatal("preparing old claim after recreation returned a non-fence error", err)
					}
					if after := rootPGImage(test, fixture.ctx, fixture.storage, fixture.repo.ID, fixture.namespace); after != before {
						test.Error("preparing an old claim changed the fresh job, lease, grants, or accounting")
					}
				}
				if stale != nil {
					if !fixture.claim.LeaseUntil.After(time.Now()) || !current.LeaseUntil.After(time.Now()) {
						test.Fatal("ABA must be rejected while both original and current leases remain live")
					}
					candidate := fixture.claim
					if phase == "old-prepared-current-claim" {
						candidate = current
					}
					err = stale.Complete(fixture.ctx, candidate, time.Now())
					if !errors.Is(err, domain.ErrConflict) {
						test.Errorf("stale completion crossed repository lifetime UUIDs %s -> %s at identical job ID %s/version 1: got %v, want conflict", fixture.epoch, epoch, current.ID, err)
					}
				}
				after, readErr := fixture.storage.GetDocJob(fixture.ctx, fixture.repo.ID, current.ID)
				if readErr != nil || !reflect.DeepEqual(after, current) {
					test.Errorf("stale publication consumed or changed fresh receipt: state=%q version=%d lease=%s; want state=%q version=%d lease=%s; read error=%v", after.State, after.Version, after.LeaseUntil, current.State, current.Version, current.LeaseUntil, readErr)
				}
				if image := rootPGImage(test, fixture.ctx, fixture.storage, fixture.repo.ID, fixture.namespace); image != before {
					test.Error("stale publication changed fresh repository ownership, raw bytes, accounting, indexes, or receipt")
				}
				if afterEpoch := rootRepoCascadeEpochPG(test, fixture, fixture.repo.ID); afterEpoch != epoch {
					test.Error("stale publication changed the fresh repository lifetime UUID", afterEpoch, epoch)
				}
				assertRootRepoCascadeUnpublishedPG(test, fixture)
				assertRootRepoCascadeSurvivorPG(test, fixture)
				if test.Failed() {
					return
				}
				fresh, err := prepareRootRepoCascadePG(test, fixture, current)
				if err != nil {
					test.Fatal("prepare current lifetime after stale publication was fenced", err)
				}
				if err := fresh.Complete(fixture.ctx, current, time.Now()); err != nil {
					test.Fatal("stale attempt consumed the current claim's ability to complete", err)
				}
				assertRootSourceWitnessPublishedPG(test, fixture.storage, fixture.ctx, current, fixture.root)
				assertRootRepoCascadeSurvivorPG(test, fixture)
			})
		}
	}
}

func TestRootRepoCascadePGPreparedCompletionCannotResurrectDeletedRepo(test *testing.T) {
	for _, root := range []struct {
		name  string
		count int
	}{{"empty", 0}, {"shared-dependencies", domain.DocRangePageEvents + 1}} {
		test.Run(root.name, func(test *testing.T) {
			fixture := newRootRepoCascadeFixturePG(test, root.count)
			publication, err := prepareRootRepoCascadePG(test, fixture, fixture.claim)
			if err != nil {
				test.Fatal(err)
			}
			deleteRootRepoCascadeOwnedFixturePG(test, fixture)
			before := rootPGImage(test, fixture.ctx, fixture.storage, fixture.repo.ID, fixture.namespace)
			if err := publication.Complete(fixture.ctx, fixture.claim, time.Now()); !errors.Is(err, domain.ErrNotFound) {
				test.Error("prepared publication after actual repository cascade must return not found", err)
			}
			assertRootRepoCascadeDeletedPG(test, fixture)
			if after := rootPGImage(test, fixture.ctx, fixture.storage, fixture.repo.ID, fixture.namespace); after != before {
				test.Error("completion after cascade changed deleted repository's archive, grants, accounting, indexes, or receipt")
			}
		})
	}
}
