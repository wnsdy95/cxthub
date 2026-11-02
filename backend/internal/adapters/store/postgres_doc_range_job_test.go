//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func docRangeJobPGRepo(t *testing.T, storage *PostgresStore, ctx context.Context) (domain.ContentHash, string) {
	t.Helper()
	repo, namespace := rootPGRepo(t, ctx, storage)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, `DELETE FROM doc_range_jobs_v1 WHERE repo_id=$1`, repo); err != nil {
			t.Error(err)
		}
	})
	return repo, namespace
}

func docRangeJobPGSource(t *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, events int) rootReaderFixture {
	t.Helper()
	fixture := rangeFixture(t, events)
	seedRootPG(t, ctx, storage, repo, fixture)
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: fixture.hash, RepoID: repo, DocHash: fixture.hash, DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func docRangeJobPGGet(t *testing.T, storage *PostgresStore, ctx context.Context, repo, hash domain.ContentHash) domain.DocRangeIndexJob {
	t.Helper()
	job, err := scanDocRangeJob(storage.pool.QueryRow(ctx, `SELECT `+docRangeJobColumns+` FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, repo, hash))
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func docRangeJobPGReceipt(job domain.DocRangeIndexJob) domain.DocRangeIndexJob {
	job.RepositoryEpoch = ""
	return job
}

func docRangeJobPGClaim(t *testing.T, storage *PostgresStore, ctx context.Context, repo, hash domain.ContentHash) domain.DocRangeIndexJob {
	t.Helper()
	job, err := storage.ClaimDocRangeJob(ctx, time.Now().UTC(), time.Minute)
	if err != nil || job.RepoID != repo || job.DocHash != hash || job.State != "running" || job.Version == 0 || job.LeaseUntil.IsZero() {
		t.Fatalf("claim: %+v %v", job, err)
	}
	return job
}

func docRangeJobPGEnqueue(t *testing.T, storage *PostgresStore, ctx context.Context, repo, hash domain.ContentHash) domain.DocRangeIndexJob {
	t.Helper()
	job, err := storage.EnqueueDocRangeJob(ctx, repo, hash, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestDocRangeJobPGReadinessIsOwnedPresenceNotProof(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	foreign, _ := docRangeJobPGRepo(t, storage, ctx)
	fixture := docRangeJobPGSource(t, storage, ctx, repo, 129)
	if ready, err := storage.HasDocRangeIndex(ctx, repo, fixture.hash); err != nil || ready {
		t.Fatal("cold readiness", ready, err)
	}
	if ready, err := storage.HasDocRangeIndex(ctx, foreign, fixture.hash); !errors.Is(err, domain.ErrNotFound) || ready {
		t.Fatal("foreign readiness", ready, err)
	}
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_generations_v2 SET descriptor=$2 WHERE doc_hash=$1`, fixture.hash, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		ready, err := storage.HasDocRangeIndex(read, repo, fixture.hash)
		if err != nil || !ready {
			t.Fatal("readiness loaded or verified metadata", ready, err)
		}
		if _, err := storage.OpenDocRangeGeneration(read, repo, fixture.hash); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("reader trusted presence as proof")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDocRangeJobPGEnqueueRequiresMatchingOwnedTypedSnapshot(t *testing.T) {
	for _, source := range []string{"missing snapshot", "foreign ownership", "mismatched document"} {
		t.Run(source, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := docRangeJobPGRepo(t, storage, ctx)
			fixture := rangeFixture(t, 1)
			seedRootPG(t, ctx, storage, repo, fixture)
			if source != "missing snapshot" {
				identity := domain.DocumentIdentityRootV1
				if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: fixture.hash, RepoID: repo, DocHash: fixture.hash, DocIdentity: identity, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
					t.Fatal(err)
				}
			}
			switch source {
			case "foreign ownership":
				if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, fixture.hash); err != nil {
					t.Fatal(err)
				}
			case "mismatched document":
				other := docRangeJobPGSource(t, storage, ctx, repo, 2)
				if _, err := storage.pool.Exec(ctx, `UPDATE snapshots SET doc_hash=$3 WHERE repo_id=$1 AND id=$2`, repo, fixture.hash, other.hash); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := storage.EnqueueDocRangeJob(ctx, repo, fixture.hash, time.Now().UTC()); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("invalid source admitted", err)
			}
			var count int
			if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1`, repo).Scan(&count); err != nil || count != 0 {
				t.Fatal("rejected source left a job", count, err)
			}
			if _, err := storage.pool.Exec(ctx, `INSERT INTO doc_range_jobs_v1(repo_id,doc_hash,state,created_at,next_attempt)
 VALUES($1,$2,'waiting',clock_timestamp(),clock_timestamp())`, repo, fixture.hash); err != nil {
				t.Fatal(err)
			}
			prior := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash)
			if _, err := storage.EnqueueDocRangeJob(ctx, repo, fixture.hash, time.Now().UTC()); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("stale receipt bypassed source ownership or root snapshot", err)
			}
			if current := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash); !reflect.DeepEqual(prior, current) {
				t.Fatal("invalid source changed stale receipt", current)
			}
		})
	}
}

func TestDocRangeJobPGLegacyMetadataCannotConvertRootSource(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	fixture := docRangeJobPGSource(test, storage, ctx, repo, 1)
	docRangeJobPGDeclareLegacy(test, storage, ctx, repo, fixture.hash)
	docRangeJobPGEnqueue(test, storage, ctx, repo, fixture.hash)
	job := docRangeJobPGClaim(test, storage, ctx, repo, fixture.hash)
	if err := storage.BuildDocRangeJob(ctx, job); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("legacy metadata converted a Root source", err)
	}
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash).Scan(&count); err != nil || count != 0 {
		test.Fatal("mislabelled source published range metadata", count, err)
	}
}

func docRangeJobPGDeclareLegacy(test *testing.T, storage *PostgresStore, ctx context.Context, repo, hash domain.ContentHash) {
	test.Helper()
	if _, err := storage.pool.Exec(ctx, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`, repo, hash); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: hash, RepoID: repo, DocHash: hash, DocIdentity: domain.DocumentIdentityLegacy,
		Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
}

func TestDocRangeJobPGEnqueueReceiptsSkipSourceAndQueueLocks(t *testing.T) {
	for _, state := range []string{"waiting", "running", "retrying", "rejected", "completed"} {
		t.Run(state, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := docRangeJobPGRepo(t, storage, ctx)
			fixture := docRangeJobPGSource(t, storage, ctx, repo, 1)
			docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
			if state != "waiting" {
				claim := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
				switch state {
				case "retrying", "rejected":
					claim.State, claim.Reason = state, "invalid_document"
					if err := storage.FinishDocRangeJob(ctx, claim, time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
				case "completed":
					if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
						t.Fatal(err)
					}
				}
			}
			prior := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash)
			blocker, err := storage.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackPG(blocker)
			if _, err := blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "cxthub-doc-range-enqueue:"+string(repo)); err != nil {
				t.Fatal(err)
			}
			var present int
			if err := blocker.QueryRow(ctx, docRangeJobSourceSQL+` FOR UPDATE OF snapshot,owned`, repo, fixture.hash, domain.DocumentIdentityRootV1).Scan(&present); err != nil {
				t.Fatal(err)
			}
			if err := blocker.QueryRow(ctx, `SELECT 1 FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' FOR UPDATE`, repo, fixture.hash).Scan(&present); err != nil {
				t.Fatal(err)
			}
			poll, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			receipt, err := storage.EnqueueDocRangeJob(poll, repo, fixture.hash, time.Now().UTC())
			if err != nil || !reflect.DeepEqual(receipt, prior) {
				t.Fatalf("receipt waited for locks or changed: %+v %v", receipt, err)
			}
		})
	}
}

func TestDocRangeJobPGConcurrentEnqueueAndCap(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	fixture := docRangeJobPGSource(t, storage, ctx, repo, 1)
	peer, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	type enqueueResult struct {
		job domain.DocRangeIndexJob
		err error
	}
	results := make(chan enqueueResult, 40)
	now := time.Now().UTC().Add(-time.Hour)
	start := make(chan struct{})
	for index := range 12 {
		go func(index int) {
			<-start
			owner := []*PostgresStore{storage, peer}[index%2]
			job, err := owner.EnqueueDocRangeJob(ctx, repo, fixture.hash, now)
			results <- enqueueResult{job, err}
		}(index)
	}
	close(start)
	for range 12 {
		result := <-results
		if result.err != nil || result.job.State != "waiting" || result.job.Version != 0 || result.job.Attempts != 0 {
			t.Fatalf("duplicate enqueue: %+v", result)
		}
	}
	var hashes []domain.ContentHash
	for range domain.MaxPendingDocRangeJobs + 4 {
		hashes = append(hashes, docRangeJobPGSource(t, storage, ctx, repo, 1).hash)
	}
	start = make(chan struct{})
	for index, hash := range hashes {
		go func(index int, hash domain.ContentHash) {
			<-start
			owner := []*PostgresStore{storage, peer}[index%2]
			job, err := owner.EnqueueDocRangeJob(ctx, repo, hash, now.Add(time.Second))
			results <- enqueueResult{job, err}
		}(index, hash)
	}
	close(start)
	accepted, rejected := 0, 0
	for range hashes {
		result := <-results
		switch {
		case result.err == nil:
			accepted++
		case errors.Is(result.err, domain.ErrResourceOverloaded):
			rejected++
		default:
			t.Fatal("unexpected enqueue error", result.err)
		}
	}
	var pending int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1 AND state IN ('waiting','running','retrying')`, repo).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != domain.MaxPendingDocRangeJobs || accepted != domain.MaxPendingDocRangeJobs-1 || rejected != 5 {
		t.Fatal("cap was not atomic", pending, accepted, rejected)
	}
	if job := docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash); job.Version != 0 {
		t.Fatal("full queue rejected or reset duplicate", job)
	}
	claim := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
	claim.State, claim.Reason = "rejected", "invalid_document"
	if err := storage.FinishDocRangeJob(ctx, claim, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if job := docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash); job.State != "rejected" || job.Reason != "invalid_document" || job.Version != claim.Version {
		t.Fatal("duplicate revived rejection", job)
	}
	additional := docRangeJobPGSource(t, storage, ctx, repo, 1)
	docRangeJobPGEnqueue(t, storage, ctx, repo, additional.hash)
}

func TestDocRangeJobPGClaimsSkipLockedAndSerializeRepository(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	otherRepo, _ := docRangeJobPGRepo(t, storage, ctx)
	first := docRangeJobPGSource(t, storage, ctx, repo, 1)
	second := docRangeJobPGSource(t, storage, ctx, repo, 1)
	other := docRangeJobPGSource(t, storage, ctx, otherRepo, 1)
	for _, source := range []struct{ repo, hash domain.ContentHash }{{repo, first.hash}, {repo, second.hash}, {otherRepo, other.hash}} {
		docRangeJobPGEnqueue(t, storage, ctx, source.repo, source.hash)
	}
	blocker, err := storage.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(blocker)
	var present int
	if err := blocker.QueryRow(ctx, `SELECT 1 FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 FOR UPDATE`, repo, first.hash).Scan(&present); err != nil {
		t.Fatal(err)
	}
	peer, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	type claimResult struct {
		job domain.DocRangeIndexJob
		err error
	}
	results := make(chan claimResult, 12)
	start := make(chan struct{})
	for index := range 12 {
		go func(index int) {
			<-start
			job, err := []*PostgresStore{storage, peer}[index%2].ClaimDocRangeJob(ctx, time.Now().UTC(), time.Minute)
			results <- claimResult{job, err}
		}(index)
	}
	close(start)
	claims := map[domain.ContentHash]domain.DocRangeIndexJob{}
	for range 12 {
		result := <-results
		if errors.Is(result.err, domain.ErrNotFound) {
			continue
		}
		if result.err != nil || result.job.DocHash == first.hash || claims[result.job.RepoID].Version != 0 {
			t.Fatalf("concurrent claim: %+v", result)
		}
		claims[result.job.RepoID] = result.job
	}
	if len(claims) != 2 || claims[repo].DocHash != second.hash || claims[otherRepo].DocHash != other.hash {
		t.Fatal("skip locked did not preserve independent progress", claims)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.ClaimDocRangeJob(ctx, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("second job ran for active repository", err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1 AND doc_hash=$2`, repo, second.hash); err != nil {
		t.Fatal(err)
	}
	recovered := docRangeJobPGClaim(t, peer, ctx, repo, second.hash)
	if recovered.Version <= claims[repo].Version || recovered.Attempts != claims[repo].Attempts+1 {
		t.Fatal("expired worker was not fenced", recovered)
	}
	if err := storage.BuildDocRangeJob(ctx, claims[repo]); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("replaced worker was allowed to build", err)
	}
}

func TestDocRangeJobPGLeaseRenewalRetryAndRejection(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	fixture := docRangeJobPGSource(t, storage, ctx, repo, 1)
	docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
	claim := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
	duplicate := docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
	if duplicate.Version != claim.Version || duplicate.State != "running" || !duplicate.LeaseUntil.Equal(claim.LeaseUntil) {
		t.Fatal("enqueue reset running claim", duplicate)
	}
	if err := storage.RenewDocRangeJob(ctx, claim, time.Now().UTC(), 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	renewed := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash)
	if !renewed.LeaseUntil.After(claim.LeaseUntil) || renewed.Version != claim.Version || renewed.Attempts != claim.Attempts {
		t.Fatal("renewal changed fence or did not extend lease", renewed)
	}
	retry := claim
	retry.State, retry.Reason, retry.NextAttempt = "retrying", "temporary_failure", time.Now().UTC().Add(time.Hour)
	if err := storage.FinishDocRangeJob(ctx, retry, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	duplicate = docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
	if duplicate.State != "retrying" || duplicate.Version != claim.Version || duplicate.Reason != retry.Reason || !duplicate.NextAttempt.Equal(retry.NextAttempt.Truncate(time.Microsecond)) || !duplicate.LeaseUntil.IsZero() {
		t.Fatal("enqueue reset retry", duplicate)
	}
	if _, err := storage.ClaimDocRangeJob(ctx, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("retry became due early", err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET next_attempt=clock_timestamp()-interval '1 hour' WHERE repo_id=$1 AND doc_hash=$2`, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	next := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
	if next.Version <= claim.Version || next.Attempts != claim.Attempts+1 {
		t.Fatal("retry did not advance fence", next)
	}
	if err := storage.RenewDocRangeJob(ctx, claim, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale renew succeeded", err)
	}
	if err := storage.FinishDocRangeJob(ctx, retry, time.Now().UTC()); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale finish succeeded", err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1 AND doc_hash=$2`, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	if err := storage.RenewDocRangeJob(ctx, next, past, 2*time.Hour); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("past caller clock revived expired claim", err)
	}
	if err := storage.BuildDocRangeJob(ctx, next); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("expired build succeeded", err)
	}
	rejected := next
	rejected.State, rejected.Reason = "rejected", "invalid_document"
	if err := storage.FinishDocRangeJob(ctx, rejected, past); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("past caller clock finished expired claim", err)
	}
	recovered := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
	recovered.State, recovered.Reason = "rejected", "invalid_document"
	if err := storage.FinishDocRangeJob(ctx, recovered, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	duplicate = docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
	if duplicate.State != "rejected" || duplicate.Reason != recovered.Reason || duplicate.Version != recovered.Version {
		t.Fatal("rejection was not durable", duplicate)
	}
}

func TestDocRangeJobPGPublishesOnlyDerivedStateAndRequeuesAfterRestart(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, namespace := docRangeJobPGRepo(t, storage, ctx)
	fixture := docRangeJobPGSource(t, storage, ctx, repo, 257)
	docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
	claim := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
	before := rootPGImage(t, ctx, storage, repo, namespace)
	if err := storage.WithinRepository(ctx, repo, func(borrowed context.Context) error {
		if err := storage.BuildDocRangeJob(borrowed, claim); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("build entered caller transaction", err)
		}
		return storage.BuildDocRangeJob(ctx, claim)
	}); err != nil {
		t.Fatal("derived build needed repository writer lock", err)
	}
	header := assertRangePages(t, ctx, storage, repo, fixture)
	completed := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash)
	if completed.State != "completed" || completed.Version != claim.Version || !completed.LeaseUntil.IsZero() {
		t.Fatal("index and completion were not published together", completed)
	}
	if after := rootPGImage(t, ctx, storage, repo, namespace); after != before {
		t.Fatal("derived build changed authoritative storage")
	}
	if err := storage.BuildDocRangeJob(ctx, claim); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("completed claim reused", err)
	}
	if duplicate := docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash); duplicate.State != "completed" || duplicate.Version != completed.Version {
		t.Fatal("present index requeued", duplicate)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash); err != nil {
		t.Fatal(err)
	}
	peer, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	requeued := docRangeJobPGEnqueue(t, peer, ctx, repo, fixture.hash)
	if requeued.State != "waiting" || requeued.Version <= completed.Version {
		t.Fatal("deleted derived index did not requeue", requeued)
	}
	claim = docRangeJobPGClaim(t, peer, ctx, repo, fixture.hash)
	if err := peer.BuildDocRangeJob(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if rebuilt := assertRangePages(t, ctx, peer, repo, fixture); !reflect.DeepEqual(header, rebuilt) {
		t.Fatal("rebuild changed selected-page meaning")
	}
}

func TestDocRangeJobPGSourceFailureDoesNotRepairOrPublish(t *testing.T) {
	for _, damage := range []string{"ownership", "snapshot", "root", "chunk ownership", "chunk bytes", "generation descriptor"} {
		t.Run(damage, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, namespace := docRangeJobPGRepo(t, storage, ctx)
			fixture := docRangeJobPGSource(t, storage, ctx, repo, 129)
			docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
			claim := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
			var err error
			missing := false
			switch damage {
			case "ownership":
				missing = true
				_, err = storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, fixture.hash)
			case "snapshot":
				missing = true
				_, err = storage.pool.Exec(ctx, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`, repo, fixture.hash)
			case "root":
				_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, []byte("corrupt root"))
			case "chunk ownership":
				missing = true
				_, err = storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, fixture.manifest.Chunks[0].Hash)
			case "chunk bytes":
				_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.manifest.Chunks[0].Hash, []byte("corrupt chunk"))
			case "generation descriptor":
				_, err = storage.pool.Exec(ctx, `INSERT INTO doc_range_generations_v2(doc_hash,source_version,generation_hash,descriptor)
 SELECT hash,doc_range_source_version,$2,$3 FROM blobs WHERE hash=$1`, fixture.hash, domain.HashContent([]byte("{}")), []byte("{}"))
			}
			if err != nil {
				t.Fatal(err)
			}
			before := rootPGImage(t, ctx, storage, repo, namespace)
			counts := rangeCountsPG(t, storage, ctx, fixture.hash)
			if err := storage.BuildDocRangeJob(ctx, claim); err == nil || missing && !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("damaged source built", err)
			}
			if got := rangeCountsPG(t, storage, ctx, fixture.hash); got != counts {
				t.Fatal("failed validation changed derived state", got, counts)
			}
			if after := rootPGImage(t, ctx, storage, repo, namespace); after != before {
				t.Fatal("builder repaired or restored source ownership")
			}
			claim.State, claim.Reason = "rejected", "invalid_document"
			if missing {
				claim.Reason = "source_unavailable"
			}
			if err := storage.FinishDocRangeJob(ctx, claim, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if rejected := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash); rejected.State != "rejected" || rejected.Reason != claim.Reason {
				t.Fatal("failure reason was not retained", rejected)
			}
		})
	}
}

type docRangeJobFenceTracer struct {
	paused           atomic.Bool
	statement        string
	reached, proceed chan struct{}
}

func (trace *docRangeJobFenceTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	matches := strings.HasPrefix(query.SQL, "SELECT 1 FROM doc_range_jobs_v1") && strings.HasSuffix(query.SQL, "FOR UPDATE")
	if trace.statement != "" {
		matches = query.SQL == trace.statement
	}
	if matches && trace.paused.CompareAndSwap(false, true) {
		close(trace.reached)
		select {
		case <-trace.proceed:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (*docRangeJobFenceTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestDocRangeJobPGEnqueueRechecksReceiptAfterAdvisoryLock(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	fixture := docRangeJobPGSource(t, storage, ctx, repo, 129)
	coldTrace := &docRangeJobFenceTracer{
		statement: `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		reached:   make(chan struct{}), proceed: make(chan struct{}),
	}
	coldConfig := storage.pool.Config()
	coldConfig.ConnConfig.Tracer = coldTrace
	coldPool, err := pgxpool.NewWithConfig(ctx, coldConfig)
	if err != nil {
		t.Fatal(err)
	}
	coldStore := &PostgresStore{pool: coldPool}
	defer coldStore.Close()
	pending, cancelPending := context.WithCancel(ctx)
	defer cancelPending()
	var coldResumed sync.Once
	resumeCold := func() { coldResumed.Do(func() { close(coldTrace.proceed) }) }
	defer resumeCold()
	type enqueueResult struct {
		job domain.DocRangeIndexJob
		err error
	}
	receipts := make(chan enqueueResult, 1)
	go func() {
		job, err := coldStore.EnqueueDocRangeJob(pending, repo, fixture.hash, time.Now().UTC())
		receipts <- enqueueResult{job, err}
	}()
	select {
	case <-coldTrace.reached:
	case receipt := <-receipts:
		t.Fatal("cold enqueue did not pause after initial miss", receipt)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
	claim := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
	buildTrace := &docRangeJobFenceTracer{reached: make(chan struct{}), proceed: make(chan struct{})}
	buildConfig := storage.pool.Config()
	buildConfig.ConnConfig.Tracer = buildTrace
	buildPool, err := pgxpool.NewWithConfig(ctx, buildConfig)
	if err != nil {
		t.Fatal(err)
	}
	builder := &PostgresStore{pool: buildPool}
	defer builder.Close()
	work, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	var buildResumed sync.Once
	resumeBuild := func() { buildResumed.Do(func() { close(buildTrace.proceed) }) }
	defer resumeBuild()
	done := make(chan error, 1)
	go func() { done <- builder.BuildDocRangeJob(work, claim) }()
	select {
	case <-buildTrace.reached:
	case err := <-done:
		t.Fatal("build did not reach final fence", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var present int
	err = storage.pool.QueryRow(ctx, `SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2 FOR UPDATE NOWAIT`, repo, fixture.hash).Scan(&present)
	var locked *pgconn.PgError
	if !errors.As(err, &locked) || locked.Code != "55P03" {
		t.Fatal("builder did not retain source ownership lock", err)
	}
	resumeCold()
	responsive, stopWaiting := context.WithTimeout(ctx, 2*time.Second)
	defer stopWaiting()
	select {
	case receipt := <-receipts:
		expected := claim
		expected.RepositoryEpoch = ""
		if receipt.err != nil || !reflect.DeepEqual(receipt.job, expected) {
			t.Fatal("cold enqueue did not return current running receipt", receipt)
		}
	case <-responsive.Done():
		t.Fatal("cold enqueue waited for source lock after advisory acquisition", responsive.Err())
	}
	select {
	case err := <-done:
		t.Fatal("builder resumed before cold enqueue returned", err)
	default:
	}
	expected := claim
	expected.RepositoryEpoch = ""
	if current := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash); !reflect.DeepEqual(current, expected) {
		t.Fatal("cold enqueue changed active claim", current)
	}
	resumeBuild()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertRangePages(t, ctx, storage, repo, fixture)
}

func TestDocRangeJobPGEnqueueReceiptsDoNotWaitForBuild(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	foreign, _ := docRangeJobPGRepo(t, storage, ctx)
	fixture := docRangeJobPGSource(t, storage, ctx, repo, 129)
	docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
	claim := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
	disjoint := docRangeJobPGSource(t, storage, ctx, repo, 1)
	waiting := docRangeJobPGEnqueue(t, storage, ctx, repo, disjoint.hash)
	newSource := docRangeJobPGSource(t, storage, ctx, repo, 2)
	trace := &docRangeJobFenceTracer{reached: make(chan struct{}), proceed: make(chan struct{})}
	config := storage.pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	builder := &PostgresStore{pool: pool}
	defer builder.Close()
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	var resumed sync.Once
	resume := func() { resumed.Do(func() { close(trace.proceed) }) }
	defer resume()
	done := make(chan error, 1)
	go func() { done <- builder.BuildDocRangeJob(work, claim) }()
	select {
	case <-trace.reached:
	case err := <-done:
		t.Fatal("build did not reach final fence", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, relation := range []string{"root", "owned"} {
		var present int
		err := storage.pool.QueryRow(ctx, `SELECT 1 FROM blobs root JOIN repo_blobs owned ON owned.hash=root.hash AND owned.kind='doc'
 WHERE owned.repo_id=$1 AND root.hash=$2 FOR UPDATE OF `+relation+` NOWAIT`, repo, fixture.hash).Scan(&present)
		var locked *pgconn.PgError
		if !errors.As(err, &locked) || locked.Code != "55P03" {
			t.Fatal("builder did not retain root and ownership locks", err)
		}
	}
	poll, stopPolling := context.WithTimeout(ctx, 2*time.Second)
	defer stopPolling()
	results := make(chan error, 8)
	start := make(chan struct{})
	for index := range 8 {
		expected := []domain.DocRangeIndexJob{claim, waiting}[index%2]
		expected.RepositoryEpoch = ""
		go func() {
			<-start
			for range 4 {
				receipt, err := storage.EnqueueDocRangeJob(poll, repo, expected.DocHash, time.Now().UTC())
				if err != nil || !reflect.DeepEqual(receipt, expected) {
					results <- fmt.Errorf("poll receipt %+v: %v", receipt, err)
					return
				}
			}
			results <- nil
		}()
	}
	close(start)
	created, err := storage.EnqueueDocRangeJob(poll, repo, newSource.hash, time.Now().UTC())
	if err != nil || created.State != "waiting" {
		t.Fatal("polls blocked disjoint source enqueue", created, err)
	}
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal("poll waited for bulk build", err)
		}
	}
	if _, err := storage.EnqueueDocRangeJob(poll, foreign, fixture.hash, time.Now().UTC()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("foreign caller received build receipt", err)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, disjoint.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.EnqueueDocRangeJob(poll, repo, disjoint.hash, time.Now().UTC()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("revoked source received stale receipt", err)
	}
	if current := docRangeJobPGGet(t, storage, ctx, repo, disjoint.hash); !reflect.DeepEqual(current, waiting) {
		t.Fatal("revoked source changed queue state", current)
	}
	select {
	case err := <-done:
		t.Fatal("builder resumed before polls finished", err)
	default:
	}
	resume()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertRangePages(t, ctx, storage, repo, fixture)
}

func TestDocRangeJobPGBuildFenceRollbackAndConcurrentRenewal(t *testing.T) {
	for _, change := range []string{"renew", "expired", "reclaimed", "rejected", "canceled"} {
		t.Run(change, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := docRangeJobPGRepo(t, storage, ctx)
			fixture := docRangeJobPGSource(t, storage, ctx, repo, 129)
			docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
			claim := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
			trace := &docRangeJobFenceTracer{reached: make(chan struct{}), proceed: make(chan struct{})}
			config := storage.pool.Config()
			config.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			builder := &PostgresStore{pool: pool}
			defer builder.Close()
			work, cancel := context.WithCancel(ctx)
			defer cancel()
			var resumed sync.Once
			resume := func() { resumed.Do(func() { close(trace.proceed) }) }
			defer resume()
			done := make(chan error, 1)
			go func() { done <- builder.BuildDocRangeJob(work, claim) }()
			select {
			case <-trace.reached:
			case err := <-done:
				t.Fatal("build did not reach final fence", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if counts := rangeCountsPG(t, storage, ctx, fixture.hash); counts != [2]int{} {
				t.Fatal("unfenced index became visible", counts)
			}
			if counts := generationCountsPG(t, storage, ctx, fixture.hash); counts != [2]int{} {
				t.Fatal("unfenced generation became visible", counts)
			}
			if err := storage.RenewDocRangeJob(ctx, claim, time.Now().UTC(), 2*time.Minute); err != nil {
				t.Fatal("validation held lease row or prevented renewal", err)
			}
			switch change {
			case "renew":
				for _, hash := range []domain.ContentHash{fixture.hash, fixture.manifest.Chunks[0].Hash} {
					var present int
					err := storage.pool.QueryRow(ctx, `SELECT 1 FROM blobs WHERE hash=$1 FOR UPDATE NOWAIT`, hash).Scan(&present)
					var locked *pgconn.PgError
					if !errors.As(err, &locked) || locked.Code != "55P03" {
						t.Fatal("source was not retained through publication", err)
					}
				}
			case "expired", "reclaimed":
				if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1 AND doc_hash=$2`, repo, fixture.hash); err != nil {
					t.Fatal(err)
				}
				if change == "reclaimed" {
					replacement := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
					if replacement.Version <= claim.Version {
						t.Fatal("replacement did not advance version")
					}
				}
			case "rejected":
				rejection := claim
				rejection.State, rejection.Reason = "rejected", "invalid_document"
				if err := storage.FinishDocRangeJob(ctx, rejection, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			resume()
			err = <-done
			if change == "renew" {
				if err != nil {
					t.Fatal("renewed build failed", err)
				}
				assertRangePages(t, ctx, storage, repo, fixture)
				if completed := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash); completed.State != "completed" {
					t.Fatal("renewed job did not complete", completed)
				}
				return
			}
			if change == "canceled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation lost", err)
				}
			} else if !errors.Is(err, domain.ErrConflict) {
				t.Fatal("stale build was not fenced", err)
			}
			if counts := rangeCountsPG(t, storage, ctx, fixture.hash); counts != [2]int{} {
				t.Fatal("failed final fence committed derived index", counts)
			}
			if counts := generationCountsPG(t, storage, ctx, fixture.hash); counts != [2]int{} {
				t.Fatal("failed final fence committed generation", counts)
			}
			if stored := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash); stored.State == "completed" {
				t.Fatal("failed build marked completed", stored)
			}
		})
	}
}

func TestDocRangeJobPGBorrowedForeignAndCanceledContexts(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	fixture := docRangeJobPGSource(t, storage, ctx, repo, 1)
	docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
	claim := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
	peer, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	check := func(bound context.Context, target *PostgresStore, expected error) {
		t.Helper()
		for name, run := range map[string]func() error{
			"enqueue": func() error {
				_, err := target.EnqueueDocRangeJob(bound, repo, fixture.hash, time.Now().UTC())
				return err
			},
			"claim": func() error {
				_, err := target.ClaimDocRangeJob(bound, time.Now().UTC(), time.Minute)
				return err
			},
			"renew": func() error { return target.RenewDocRangeJob(bound, claim, time.Now().UTC(), time.Minute) },
			"build": func() error { return target.BuildDocRangeJob(bound, claim) },
			"finish": func() error {
				rejected := claim
				rejected.State, rejected.Reason = "rejected", "source_unavailable"
				return target.FinishDocRangeJob(bound, rejected, time.Now().UTC())
			},
		} {
			if err := run(); !errors.Is(err, expected) {
				t.Fatalf("%s borrowed or canceled operation: %v", name, err)
			}
		}
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		check(read, storage, domain.ErrConflict)
		check(read, peer, domain.ErrConflict)
		if _, err := peer.HasDocRangeIndex(read, repo, fixture.hash); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("foreign readiness escaped transaction", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		check(write, storage, domain.ErrConflict)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	check(outbound.WithDocReadOnly(ctx), storage, domain.ErrConflict)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	check(canceled, storage, context.Canceled)
	if _, err := storage.HasDocRangeIndex(canceled, repo, fixture.hash); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if counts := rangeCountsPG(t, storage, ctx, fixture.hash); counts != [2]int{} {
		t.Fatal("rejected contexts published state", counts)
	}
	if current := docRangeJobPGGet(t, storage, ctx, repo, fixture.hash); current.State != "running" || current.Version != claim.Version {
		t.Fatal(fmt.Sprint("rejected contexts changed claim: ", current))
	}
}
