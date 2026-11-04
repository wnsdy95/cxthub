//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocRangeBoundaryPGActiveQueueIndexAndBound(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	var column, predicate string
	if err := storage.pool.QueryRow(ctx, `SELECT pg_get_indexdef(indexrelid,1,true),pg_get_expr(indpred,indrelid)
 FROM pg_index WHERE indexrelid='doc_range_jobs_v1_active_repo'::regclass`).Scan(&column, &predicate); err != nil {
		t.Fatal(err)
	}
	if column != "repo_id" || strings.Contains(predicate, "completed") {
		t.Fatal("unbounded queue index", column, predicate)
	}
	for _, state := range []string{"waiting", "running", "retrying"} {
		if !strings.Contains(predicate, state) {
			t.Fatal("active state missing from partial index", predicate)
		}
	}
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_range_jobs_v1(repo_id,doc_hash,base_hash,state,created_at,next_attempt)
 SELECT $1,$1,'sha256:'||encode(digest(ordinal::text,'sha256'),'hex'),
 CASE WHEN ordinal<=64 THEN 'waiting' ELSE 'completed' END,clock_timestamp(),clock_timestamp()
 FROM generate_series(1,10000) ordinal`, repo); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := transaction.QueryRow(ctx, docRangeJobActiveCountSQL, repo, domain.MaxPendingDocRangeJobs).Scan(&count); err != nil || count != domain.MaxPendingDocRangeJobs {
		t.Fatal("admission scanned beyond cap", count, err)
	}
}

func TestDocRangeBoundaryPGQueueDedupSharedCapAndRunningScope(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	pair := docRangeBoundaryPGPair(t, storage, ctx, repo, 1, 0)
	if _, err := storage.EnqueueDocRangeJob(ctx, repo, pair[0].hash, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 12)
	for range 12 {
		go func() {
			job, err := storage.EnqueueDocRangeBoundary(ctx, repo, pair[0].hash, pair[1].hash, time.Now().Add(-time.Hour))
			if err == nil && (job.State != "waiting" || job.Version != 0 || job.BaseHash != pair[1].hash) {
				err = fmt.Errorf("unexpected receipt: %+v", job)
			}
			results <- err
		}()
	}
	for range 12 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for range domain.MaxPendingDocRangeJobs - 2 {
		base := docRangeJobPGSource(t, storage, ctx, repo, 1)
		if _, err := storage.EnqueueDocRangeBoundary(ctx, repo, pair[0].hash, base.hash, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	last := docRangeJobPGSource(t, storage, ctx, repo, 1)
	if _, err := storage.EnqueueDocRangeBoundary(ctx, repo, pair[0].hash, last.hash, time.Now()); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatal("prefix escaped combined cap", err)
	}
	if _, err := storage.EnqueueDocRangeJob(ctx, repo, last.hash, time.Now()); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatal("index escaped combined cap", err)
	}
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1`, repo).Scan(&count); err != nil || count != domain.MaxPendingDocRangeJobs {
		t.Fatal("duplicates or over-cap jobs persisted", count, err)
	}
	index := docRangeJobPGClaim(t, storage, ctx, repo, pair[0].hash)
	if index.BaseHash != "" {
		t.Fatal("wrong queue order", index)
	}
	if _, err := storage.ClaimDocRangeJob(ctx, time.Now(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("prefix ran alongside index", err)
	}
	if err := storage.BuildDocRangeJob(ctx, index); err != nil {
		t.Fatal(err)
	}
	prefix := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
	if _, err := storage.ClaimDocRangeJob(ctx, time.Now(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("different base ran alongside prefix", err)
	}
	if err := storage.RenewDocRangeJob(ctx, prefix, time.Now(), 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	prefix.State, prefix.Reason = "retrying", "temporary"
	prefix.NextAttempt = time.Now().Add(time.Hour)
	if err := storage.FinishDocRangeJob(ctx, prefix, time.Now()); err != nil {
		t.Fatal(err)
	}
	if current := docRangeBoundaryPGGet(t, storage, ctx, repo, pair); current.State != "retrying" || current.Version != prefix.Version {
		t.Fatal("prefix finish targeted another job", current)
	}
	var state string
	if err := storage.pool.QueryRow(ctx, `SELECT state FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, repo, pair[0].hash).Scan(&state); err != nil || state != "completed" {
		t.Fatal("prefix lifecycle changed index receipt", state, err)
	}
}

func TestDocRangeBoundaryPGBothScopesRequiredForEveryReceipt(t *testing.T) {
	for _, completed := range []bool{false, true} {
		for _, side := range []int{0, 1} {
			for _, damage := range []string{"grant", "snapshot", "mismatch"} {
				t.Run(fmt.Sprintf("completed=%v/side=%d/%s", completed, side, damage), func(t *testing.T) {
					storage, ctx := chunkReusePG(t)
					repo, _ := docRangeJobPGRepo(t, storage, ctx)
					pair := docRangeBoundaryPGPair(t, storage, ctx, repo, 1, 0)
					docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
					if completed {
						claim := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
						if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
							t.Fatal(err)
						}
					}
					before := docRangeBoundaryPGGet(t, storage, ctx, repo, pair)
					statement, args := `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, []any{repo, pair[side].hash}
					switch damage {
					case "snapshot":
						statement = `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`
					case "mismatch":
						statement, args = `UPDATE snapshots SET doc_hash=$3 WHERE repo_id=$1 AND id=$2`, append(args, pair[1-side].hash)
					}
					if _, err := storage.pool.Exec(ctx, statement, args...); err != nil {
						t.Fatal(err)
					}
					docRangeBoundaryPGRead(t, storage, ctx, repo, pair, 0, domain.ErrNotFound)
					if _, err := storage.EnqueueDocRangeBoundary(ctx, repo, pair[0].hash, pair[1].hash, time.Now()); !errors.Is(err, domain.ErrNotFound) {
						t.Fatal("invalid scope returned receipt", err)
					}
					if current := docRangeBoundaryPGGet(t, storage, ctx, repo, pair); !reflect.DeepEqual(before, current) {
						t.Fatal("scope failure modified receipt", current)
					}
				})
			}
		}
	}
}

func TestDocRangeBoundaryPGLegacyDeclarationCannotReuseRootEvidence(test *testing.T) {
	for _, side := range []int{0, 1} {
		test.Run(fmt.Sprint(side), func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			repo, _ := docRangeJobPGRepo(test, storage, ctx)
			pair := docRangeBoundaryPGPair(test, storage, ctx, repo, 1, 0)
			docRangeBoundaryPGEnqueue(test, storage, ctx, repo, pair)
			claim := docRangeBoundaryPGClaim(test, storage, ctx, repo, pair)
			if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
				test.Fatal(err)
			}
			before := docRangeBoundaryPGGet(test, storage, ctx, repo, pair)
			docRangeJobPGDeclareLegacy(test, storage, ctx, repo, pair[side].hash)
			docRangeBoundaryPGRead(test, storage, ctx, repo, pair, 0, domain.ErrIntegrity)
			if _, err := storage.EnqueueDocRangeBoundary(ctx, repo, pair[0].hash, pair[1].hash, time.Now()); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("mislabelled legacy source reused completed Root evidence", err)
			}
			if current := docRangeBoundaryPGGet(test, storage, ctx, repo, pair); !reflect.DeepEqual(before, current) {
				test.Fatal("identity mismatch repaired a completed receipt", current)
			}
		})
	}
}

func TestDocRangeBoundaryPGEnqueueRechecksBothScopesAfterAdvisory(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(fmt.Sprintf("revoke=%v", revoke), func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := docRangeJobPGRepo(t, storage, ctx)
			pair := docRangeBoundaryPGPair(t, storage, ctx, repo, 129, 128)
			trace := &docRangeJobFenceTracer{statement: `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, reached: make(chan struct{}), proceed: make(chan struct{})}
			peer := docRangeBoundaryPGTracedStore(t, storage, ctx, trace)
			var once sync.Once
			resume := func() { once.Do(func() { close(trace.proceed) }) }
			t.Cleanup(resume)
			type result struct {
				job domain.DocRangeIndexJob
				err error
			}
			poll, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			done := make(chan result, 1)
			go func() {
				job, err := peer.EnqueueDocRangeBoundary(poll, repo, pair[0].hash, pair[1].hash, time.Now())
				done <- result{job, err}
			}()
			select {
			case <-trace.reached:
			case got := <-done:
				t.Fatal("enqueue missed advisory fence", got)
			case <-poll.Done():
				t.Fatal(poll.Err())
			}
			docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
			claim := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
			var resumeBuild func()
			var built <-chan error
			if revoke {
				if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, pair[1].hash); err != nil {
					t.Fatal(err)
				}
			} else {
				resumeBuild, _, built = docRangeBoundaryPGPause(t, storage, ctx, claim, "")
			}
			resume()
			got := <-done
			if revoke {
				if !errors.Is(got.err, domain.ErrNotFound) {
					t.Fatal("post-advisory check missed revoked base", got)
				}
			} else {
				if got.err != nil || !reflect.DeepEqual(got.job, docRangeJobPGReceipt(claim)) {
					t.Fatal("receipt blocked on root or job locks", got)
				}
				resumeBuild()
				if err := <-built; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
