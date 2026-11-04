//go:build postgres

package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func rangeJobClaimProgressImagePG(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, namespace string) [2]string {
	test.Helper()
	var image string
	if err := storage.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'jobs',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(job),job.xmin::text) ORDER BY job.doc_hash,job.base_hash)
 FROM doc_range_jobs_v1 job WHERE job.repo_id=$1),
 'catalog',(SELECT jsonb_build_array(to_jsonb(catalog),catalog.xmin::text)
 FROM repository_catalog_state catalog WHERE catalog.repo_id=$1),
 'snapshots',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(snapshot),snapshot.xmin::text) ORDER BY snapshot.id)
 FROM snapshots snapshot WHERE snapshot.repo_id=$1)
 )::text`, repo).Scan(&image); err != nil {
		test.Fatal("capture range queue, lifetime, and snapshot witnesses", err)
	}
	return [2]string{rootPGImage(test, ctx, storage, repo, namespace), image}
}

func assertRangeJobClaimProgressClaimPG(test *testing.T, claimed, waiting domain.DocRangeIndexJob, epoch string, now time.Time, lease time.Duration) {
	test.Helper()
	if epoch == "" || waiting.RepositoryEpoch != "" || waiting.State != "waiting" || !waiting.LeaseUntil.IsZero() {
		test.Fatal("claim comparison requires a real catalog epoch and an unchanged unbound waiting receipt", epoch, waiting)
	}
	expected := waiting
	expected.RepositoryEpoch = epoch
	expected.State, expected.Reason = "running", ""
	expected.Attempts++
	expected.Version++
	if !claimed.LeaseUntil.Equal(now.Add(lease)) {
		test.Fatal("claim did not preserve the exact fixed-clock lease instant", claimed.LeaseUntil, now.Add(lease))
	}
	expected.LeaseUntil = claimed.LeaseUntil
	if !reflect.DeepEqual(claimed, expected) {
		test.Fatal("actual claim changed identity, delivery fields, fixed-clock lease, or repository lifetime", claimed, expected)
	}
}

func TestPGRangeJobClaimProgressPastBusyRepository(test *testing.T) {
	for _, scenario := range []string{"all-due-rows-locked", "claim-advisory-locked", "repository-row-locked", "catalog-row-locked"} {
		test.Run(scenario, func(test *testing.T) {
			database, storage := authorityScopePG(test)
			ctx := database.ctx
			var existing int
			if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_jobs_v1`).Scan(&existing); err != nil || existing != 0 {
				test.Fatal("claim progress requires an isolated empty range queue", existing, err)
			}
			frontRepo, frontNamespace := docRangeJobPGRepo(test, storage, ctx)
			behindRepo, behindNamespace := docRangeJobPGRepo(test, storage, ctx)
			liveRepo, liveNamespace := docRangeJobPGRepo(test, storage, ctx)
			frontFirst := docRangeJobPGSource(test, storage, ctx, frontRepo, 1)
			frontSecond := docRangeJobPGSource(test, storage, ctx, frontRepo, 2)
			behind := docRangeJobPGSource(test, storage, ctx, behindRepo, 3)
			live := docRangeJobPGSource(test, storage, ctx, liveRepo, 4)
			lifetime := rootRepoCascadeFixturePG{storage: storage, ctx: ctx}
			frontEpoch := rootRepoCascadeEpochPG(test, lifetime, frontRepo)
			behindEpoch := rootRepoCascadeEpochPG(test, lifetime, behindRepo)
			liveEpoch := rootRepoCascadeEpochPG(test, lifetime, liveRepo)
			now := time.Now().UTC().Truncate(time.Microsecond)
			lease := 5 * time.Minute
			liveWaiting, err := storage.EnqueueDocRangeJob(ctx, liveRepo, live.hash, now.Add(-3*time.Minute))
			if err != nil {
				test.Fatal("enqueue the independent preexisting lease", err)
			}
			liveClaim, err := storage.ClaimDocRangeJob(ctx, now, lease)
			if err != nil {
				test.Fatal("create the independent lease through the actual claimant", err)
			}
			assertRangeJobClaimProgressClaimPG(test, liveClaim, liveWaiting, liveEpoch, now, lease)
			var waiting []domain.DocRangeIndexJob
			for _, source := range []struct {
				repo domain.ContentHash
				hash domain.ContentHash
				due  time.Time
			}{
				{repo: frontRepo, hash: frontFirst.hash, due: now.Add(-2 * time.Minute)},
				{repo: frontRepo, hash: frontSecond.hash, due: now.Add(-90 * time.Second)},
				{repo: behindRepo, hash: behind.hash, due: now.Add(-time.Minute)},
			} {
				queued, err := storage.EnqueueDocRangeJob(ctx, source.repo, source.hash, source.due)
				if err != nil || queued.State != "waiting" || queued.Version != 0 || queued.Attempts != 0 || queued.RepositoryEpoch != "" || !queued.LeaseUntil.IsZero() ||
					!queued.NextAttempt.Equal(source.due) || !queued.CreatedAt.Equal(source.due) {
					test.Fatal("enqueue an explicitly ordered unbound waiting receipt", queued, source, err)
				}
				waiting = append(waiting, queued)
			}
			ordered, err := storage.pool.Query(ctx, `SELECT repo_id,doc_hash FROM doc_range_jobs_v1
 WHERE state IN ('waiting','retrying','running') AND next_attempt<=$1 AND (state<>'running' OR lease_until<=$1)
 ORDER BY next_attempt,created_at,repo_id,doc_hash,base_hash`, now)
			if err != nil {
				test.Fatal("read the actual fixed-clock candidate order", err)
			}
			defer ordered.Close()
			for _, expected := range waiting {
				if !ordered.Next() {
					test.Fatal("fixed-clock due queue ended before the two front jobs and behind job", ordered.Err())
				}
				var repo, hash domain.ContentHash
				if err := ordered.Scan(&repo, &hash); err != nil || repo != expected.RepoID || hash != expected.DocHash {
					test.Fatal("the blocked repository must precede the behind repository in the real candidate order", repo, hash, expected, err)
				}
			}
			if ordered.Next() {
				test.Fatal("unexpected due work in the isolated candidate queue")
			}
			ordered.Close()
			if err := ordered.Err(); err != nil {
				test.Fatal(err)
			}
			frontBefore := rangeJobClaimProgressImagePG(test, storage, ctx, frontRepo, frontNamespace)
			liveBefore := rangeJobClaimProgressImagePG(test, storage, ctx, liveRepo, liveNamespace)
			behindSourceBefore := rootPGImage(test, ctx, storage, behindRepo, behindNamespace)
			sources := make(map[domain.ContentHash]rootSourceWitnessStatePG)
			for _, source := range []rootReaderFixture{frontFirst, frontSecond, behind, live} {
				sources[source.hash] = readRootSourceWitnessStatePG(test, storage, ctx, source.hash)
				for hash := range source.bodies {
					sources[hash] = readRootSourceWitnessStatePG(test, storage, ctx, hash)
				}
			}
			holder := authorityScopePeerPG(test, storage, "", nil)
			claimant := authorityScopePeerPG(test, storage, "", nil)
			blocker, err := holder.pool.Begin(ctx)
			if err != nil {
				test.Fatal("begin the independent leading repository blocker", err)
			}
			defer rollbackPG(blocker)
			holderPID := blocker.Conn().PgConn().PID()
			if claimantPID := authorityScopePIDPG(test, ctx, claimant); claimantPID == holderPID {
				test.Fatal("the claimant must use a backend independent of the held lock", holderPID)
			}
			switch scenario {
			case "all-due-rows-locked":
				locked, err := blocker.Query(ctx, `SELECT doc_hash FROM doc_range_jobs_v1
 WHERE repo_id=$1 AND state='waiting' AND next_attempt<=$2
 ORDER BY next_attempt,created_at,repo_id,doc_hash,base_hash FOR UPDATE`, frontRepo, now)
				if err != nil {
					test.Fatal("acquire all actual due job row locks before invoking ClaimDocRangeJob", err)
				}
				defer locked.Close()
				for _, expected := range waiting[:2] {
					if !locked.Next() {
						test.Fatal("the leading repository must have two actually locked due jobs", locked.Err())
					}
					var hash domain.ContentHash
					if err := locked.Scan(&hash); err != nil || hash != expected.DocHash {
						test.Fatal("row lock did not cover the expected leading job", hash, expected, err)
					}
				}
				if locked.Next() {
					test.Fatal("unexpected additional due row in the leading repository")
				}
				locked.Close()
				if err := locked.Err(); err != nil {
					test.Fatal(err)
				}
			case "claim-advisory-locked":
				var held bool
				if err := blocker.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "cxthub-doc-range-claim:"+string(frontRepo)).Scan(&held); err != nil || !held {
					test.Fatal("acquire the exact production repository claim advisory lock before invoking ClaimDocRangeJob", held, err)
				}
			case "repository-row-locked":
				var retained domain.ContentHash
				if err := blocker.QueryRow(ctx, `SELECT id FROM repos WHERE id=$1 FOR UPDATE`, frontRepo).Scan(&retained); err != nil || retained != frontRepo {
					test.Fatal("hold the leading repository row against the claimant's lifetime pin", retained, err)
				}
			case "catalog-row-locked":
				var retained domain.ContentHash
				if err := blocker.QueryRow(ctx, `SELECT repo_id FROM repository_catalog_state WHERE repo_id=$1 FOR UPDATE`, frontRepo).Scan(&retained); err != nil || retained != frontRepo {
					test.Fatal("hold the leading catalog row against the claimant's lifetime pin", retained, err)
				}
			}
			claimContext, stopClaim := context.WithTimeout(ctx, 5*time.Second)
			behindClaim, err := claimant.ClaimDocRangeJob(claimContext, now, lease)
			stopClaim()
			if err != nil {
				test.Fatal("one ClaimDocRangeJob call must advance past the busy leading repository", err)
			}
			assertRangeJobClaimProgressClaimPG(test, behindClaim, waiting[2], behindEpoch, now, lease)
			if after := rangeJobClaimProgressImagePG(test, storage, ctx, frontRepo, frontNamespace); after != frontBefore {
				test.Fatal("probing a busy repository changed its actual receipts, leases, sources, ownership, accounting, or lifetime")
			}
			assertPreserved := func() {
				test.Helper()
				if after := rangeJobClaimProgressImagePG(test, storage, ctx, liveRepo, liveNamespace); after != liveBefore {
					test.Fatal("claim progress changed the independent existing actual lease or its durable witnesses")
				}
				if after := rootPGImage(test, ctx, storage, behindRepo, behindNamespace); after != behindSourceBefore {
					test.Fatal("claiming the behind job changed source bytes, ownership, accounting, or published indexes")
				}
				for hash, source := range sources {
					assertRootSourceWitnessStatePG(test, storage, ctx, hash, source)
				}
				expected := behindClaim
				expected.RepositoryEpoch = ""
				if actual := docRangeJobPGGet(test, storage, ctx, behindRepo, behind.hash); !reflect.DeepEqual(actual, expected) {
					test.Fatal("the behind repository lost its actual claim or fixed-clock lease", actual, expected)
				}
			}
			assertPreserved()
			if err := blocker.Commit(ctx); err != nil {
				test.Fatal("release the real leading repository lock only after the behind claim and no-effect checks", err)
			}
			releasedContext, stopReleased := context.WithTimeout(ctx, 5*time.Second)
			frontClaim, err := claimant.ClaimDocRangeJob(releasedContext, now, lease)
			stopReleased()
			if err != nil {
				test.Fatal("the leading repository must claim normally in one call after lock release", err)
			}
			assertRangeJobClaimProgressClaimPG(test, frontClaim, waiting[0], frontEpoch, now, lease)
			expectedFront := frontClaim
			expectedFront.RepositoryEpoch = ""
			if actual := docRangeJobPGGet(test, storage, ctx, frontRepo, frontFirst.hash); !reflect.DeepEqual(actual, expectedFront) {
				test.Fatal("released leading repository did not retain its actual new claim", actual, expectedFront)
			}
			if actual := docRangeJobPGGet(test, storage, ctx, frontRepo, frontSecond.hash); !reflect.DeepEqual(actual, waiting[1]) {
				test.Fatal("claiming the released repository also changed its second waiting job", actual, waiting[1])
			}
			assertPreserved()
		})
	}
}
