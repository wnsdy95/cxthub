//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docFindFieldsPGFixture struct {
	storage *PostgresStore
	ctx     context.Context
	repo    domain.ContentHash
	doc     domain.ContentHash
}

func docFindFieldsPG(t *testing.T) docFindFieldsPGFixture {
	t.Helper()
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, `DELETE FROM doc_find_fields_jobs_v1 WHERE repo_id=$1`, repo); err != nil {
			t.Error(err)
		}
	})
	fixture := docRangeJobPGSource(t, storage, ctx, repo, 1)
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		return storage.ensureRangeGeneration(bound, repo, fixture.hash)
	}); err != nil {
		t.Fatal(err)
	}
	return docFindFieldsPGFixture{storage: storage, ctx: ctx, repo: repo, doc: fixture.hash}
}

func docFindFieldsTestFields(t *testing.T) domain.DocFindFields {
	t.Helper()
	raw := []byte(`{"kind":"reasoning","redacted_summary":` + strconv.Quote(t.Name()) + `,"seq":1}`)
	fields, err := domain.BuildDocFindFields(domain.HashContent(raw), raw)
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

func (fixture docFindFieldsPGFixture) fields(t *testing.T) domain.DocFindFields {
	t.Helper()
	fields := docFindFieldsTestFields(t)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := fixture.storage.pool.Exec(cleanup, `DELETE FROM doc_find_fields_v1 WHERE event_hash=$1`, fields.EventHash); err != nil {
			t.Error(err)
		}
	})
	return fields
}

func (fixture docFindFieldsPGFixture) enqueue(t *testing.T) domain.DocRangeIndexJob {
	t.Helper()
	job, err := fixture.storage.EnqueueDocFindFields(fixture.ctx, fixture.repo, fixture.doc, time.Now().UTC())
	if err != nil || job.BaseHash != "" {
		t.Fatalf("enqueue: %+v %v", job, err)
	}
	return job
}

func (fixture docFindFieldsPGFixture) claim(t *testing.T) domain.DocRangeIndexJob {
	t.Helper()
	job, err := fixture.storage.ClaimDocFindFields(fixture.ctx, time.Now().UTC(), time.Minute)
	if err != nil || job.RepoID != fixture.repo || job.DocHash != fixture.doc || job.BaseHash != "" || job.State != "running" {
		t.Fatalf("claim: %+v %v", job, err)
	}
	return job
}

func (fixture docFindFieldsPGFixture) finish(t *testing.T, job domain.DocRangeIndexJob, state string, want error) {
	t.Helper()
	now := time.Now().UTC()
	job.State, job.NextAttempt = state, now
	if err := fixture.storage.FinishDocFindFields(fixture.ctx, job, now); !errors.Is(err, want) {
		t.Fatalf("finish %s: %v, want %v", state, err, want)
	}
}

func (fixture docFindFieldsPGFixture) read(t *testing.T, repo domain.ContentHash, fields domain.DocFindFields, want error) {
	t.Helper()
	err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		got, err := fixture.storage.ReadDocFindFields(read, repo, fixture.doc, fields.EventHash, fields.EventBytes)
		if !errors.Is(err, want) {
			t.Fatalf("read: %+v %v, want %v", got, err, want)
		}
		if err == nil && !reflect.DeepEqual(got, fields) {
			t.Fatalf("read fields: %+v, want %+v", got, fields)
		}
		if err != nil && !reflect.DeepEqual(got, domain.DocFindFields{}) {
			t.Fatalf("failed read returned fields: %+v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (fixture docFindFieldsPGFixture) changeSource(t *testing.T) {
	t.Helper()
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.doc); err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.WithinRepository(fixture.ctx, fixture.repo, func(bound context.Context) error {
		return fixture.storage.ensureRangeGeneration(bound, fixture.repo, fixture.doc)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDocFindFieldsPGImmutableConflict(t *testing.T) {
	fixture := docFindFieldsPG(t)
	fixture.enqueue(t)
	job := fixture.claim(t)
	fields := fixture.fields(t)
	fixture.read(t, fixture.repo, fields, domain.ErrNotFound)
	for range 2 {
		if err := fixture.storage.PutDocFindFields(fixture.ctx, job, fields); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutation := range []string{"event_bytes", "summary"} {
		changed := fields
		if mutation == "event_bytes" {
			changed.EventBytes++
		} else {
			changed.Summary = nil
		}
		if err := fixture.storage.PutDocFindFields(fixture.ctx, job, changed); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("immutable %s: %v", mutation, err)
		}
		fixture.read(t, fixture.repo, fields, nil)
	}
	fixture.finish(t, job, "completed", nil)
	if err := fixture.storage.PutDocFindFields(fixture.ctx, job, fields); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("completed worker wrote sidecar: %v", err)
	}
	fixture.read(t, fixture.repo, fields, nil)
}

func TestDocFindFieldsPGReadIntegrityAndOwnership(t *testing.T) {
	for _, mutation := range []string{"projection_hash", "event_bytes", "canonical_bytes", "ownership"} {
		t.Run(mutation, func(t *testing.T) {
			fixture := docFindFieldsPG(t)
			fixture.enqueue(t)
			job := fixture.claim(t)
			fields := fixture.fields(t)
			if err := fixture.storage.PutDocFindFields(fixture.ctx, job, fields); err != nil {
				t.Fatal(err)
			}
			fixture.read(t, fixture.repo, fields, nil)
			foreign := domain.HashContent([]byte("unowned doc find fields repo"))
			fixture.read(t, foreign, fields, domain.ErrNotFound)
			if _, err := fixture.storage.ReadDocFindFields(fixture.ctx, fixture.repo, fixture.doc, fields.EventHash, fields.EventBytes); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("read outside read-only snapshot: %v", err)
			}
			if err := fixture.storage.WithinRepository(fixture.ctx, fixture.repo, func(write context.Context) error {
				_, err := fixture.storage.ReadDocFindFields(write, fixture.repo, fixture.doc, fields.EventHash, fields.EventBytes)
				if !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("read borrowed writable transaction: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var err error
			want := domain.ErrIntegrity
			switch mutation {
			case "projection_hash":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_find_fields_v1 SET projection_hash=$2 WHERE event_hash=$1`, fields.EventHash, domain.HashContent([]byte("wrong projection")))
			case "event_bytes":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_find_fields_v1 SET event_bytes=event_bytes+1 WHERE event_hash=$1`, fields.EventHash)
			case "canonical_bytes":
				raw, encodeErr := fields.Encode()
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				raw = append(raw, ' ')
				_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_find_fields_v1 SET bytes=$2,projection_hash=$3 WHERE event_hash=$1`, fields.EventHash, raw, domain.HashContent(raw))
			case "ownership":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, fixture.doc)
				want = domain.ErrNotFound
			}
			if err != nil {
				t.Fatal(err)
			}
			fixture.read(t, fixture.repo, fields, want)
		})
	}
}

func TestDocFindFieldsPGIdempotentQueueAndCompetingClaims(t *testing.T) {
	fixture := docFindFieldsPG(t)
	queued := fixture.enqueue(t)
	if duplicate := fixture.enqueue(t); !reflect.DeepEqual(duplicate, queued) {
		t.Fatalf("non-idempotent queue: %+v %+v", queued, duplicate)
	}
	if _, err := fixture.storage.ClaimDocRangeJob(fixture.ctx, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("range worker claimed find fields queue: %v", err)
	}
	second := docRangeJobPGSource(t, fixture.storage, fixture.ctx, fixture.repo, 2)
	if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, second.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.EnqueueDocFindFields(fixture.ctx, fixture.repo, second.hash, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	type result struct {
		job domain.DocRangeIndexJob
		err error
	}
	results := make(chan result, 8)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range cap(results) {
		workers.Go(func() {
			<-start
			job, err := fixture.storage.ClaimDocFindFields(fixture.ctx, time.Now().UTC(), time.Minute)
			results <- result{job, err}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	var claimed domain.DocRangeIndexJob
	count := 0
	for result := range results {
		if result.err == nil {
			count++
			claimed = result.job
		} else if !errors.Is(result.err, domain.ErrNotFound) {
			t.Fatal(result.err)
		}
	}
	if count != 1 || claimed.RepoID != fixture.repo || claimed.Attempts != 1 || claimed.Version != queued.Version+1 {
		t.Fatalf("competing claims: count=%d job=%+v", count, claimed)
	}
	if _, err := fixture.storage.ClaimDocFindFields(fixture.ctx, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second running job in repository: %v", err)
	}
	fixture.finish(t, claimed, "completed", nil)
	next, err := fixture.storage.ClaimDocFindFields(fixture.ctx, time.Now().UTC(), time.Minute)
	if err != nil || next.DocHash == claimed.DocHash || next.RepoID != fixture.repo {
		t.Fatalf("next repository job: %+v %v", next, err)
	}
	fixture.finish(t, next, "completed", nil)
}

func TestDocFindFieldsPGExpiredLeaseAndOldVersion(t *testing.T) {
	fixture := docFindFieldsPG(t)
	fixture.enqueue(t)
	job := fixture.claim(t)
	fields := fixture.fields(t)
	if err := fixture.storage.RenewDocFindFields(fixture.ctx, job, time.Now().UTC(), 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_find_fields_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second'
 WHERE repo_id=$1 AND doc_hash=$2`, fixture.repo, fixture.doc); err != nil {
		t.Fatal(err)
	}
	checkStale := func(stale domain.DocRangeIndexJob) {
		t.Helper()
		if err := fixture.storage.PutDocFindFields(fixture.ctx, stale, fields); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("stale worker wrote: %v", err)
		}
		if err := fixture.storage.RenewDocFindFields(fixture.ctx, stale, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("stale worker renewed: %v", err)
		}
		for _, state := range []string{"completed", "retrying", "rejected", "waiting"} {
			fixture.finish(t, stale, state, domain.ErrConflict)
		}
		fixture.read(t, fixture.repo, fields, domain.ErrNotFound)
	}
	checkStale(job)
	reclaimed := fixture.claim(t)
	if reclaimed.Version != job.Version+1 || reclaimed.Attempts != job.Attempts+1 {
		t.Fatalf("reclaim did not advance fence: %+v %+v", job, reclaimed)
	}
	checkStale(job)
	if err := fixture.storage.PutDocFindFields(fixture.ctx, reclaimed, fields); err != nil {
		t.Fatal(err)
	}
	fixture.finish(t, reclaimed, "completed", nil)
	fixture.read(t, fixture.repo, fields, nil)
}

func TestDocFindFieldsPGSourceChangeRecovery(t *testing.T) {
	for _, state := range []string{"waiting", "running", "retrying", "completed", "rejected"} {
		t.Run(state, func(t *testing.T) {
			fixture := docFindFieldsPG(t)
			previous := fixture.enqueue(t)
			if state != "waiting" {
				previous = fixture.claim(t)
				if state != "running" {
					fixture.finish(t, previous, state, nil)
				}
			}
			fixture.changeSource(t)
			fields := fixture.fields(t)
			if state == "running" {
				if err := fixture.storage.PutDocFindFields(fixture.ctx, previous, fields); !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("old source worker wrote: %v", err)
				}
				fixture.finish(t, previous, "completed", domain.ErrConflict)
			}
			requeued := fixture.enqueue(t)
			if requeued.State != "waiting" || requeued.Version != previous.Version+1 || requeued.Attempts != 0 || !requeued.LeaseUntil.IsZero() {
				t.Fatalf("source change did not requeue: %+v %+v", previous, requeued)
			}
			if duplicate := fixture.enqueue(t); !reflect.DeepEqual(duplicate, requeued) {
				t.Fatalf("source change receipt not idempotent: %+v %+v", duplicate, requeued)
			}
			job := fixture.claim(t)
			if job.Version != requeued.Version+1 {
				t.Fatalf("fresh source claim version: %+v", job)
			}
			if state == "running" {
				if err := fixture.storage.PutDocFindFields(fixture.ctx, previous, fields); !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("replaced source worker wrote: %v", err)
				}
				fixture.finish(t, previous, "completed", domain.ErrConflict)
				fixture.finish(t, previous, "retrying", domain.ErrConflict)
			}
			if err := fixture.storage.PutDocFindFields(fixture.ctx, job, fields); err != nil {
				t.Fatal(err)
			}
			fixture.finish(t, job, "completed", nil)
			fixture.read(t, fixture.repo, fields, nil)
			var bound bool
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT job.source_version=root.doc_range_source_version
 FROM doc_find_fields_jobs_v1 job JOIN blobs root ON root.hash=job.doc_hash WHERE job.repo_id=$1 AND job.doc_hash=$2`, fixture.repo, fixture.doc).Scan(&bound); err != nil || !bound {
				t.Fatalf("source version not captured: %v %v", bound, err)
			}
		})
	}
}

func TestDocFindFieldsPGCompletedDemandRebuildsMissingSidecar(t *testing.T) {
	fixture := docFindFieldsPG(t)
	fixture.enqueue(t)
	job := fixture.claim(t)
	fields := fixture.fields(t)
	if err := fixture.storage.PutDocFindFields(fixture.ctx, job, fields); err != nil {
		t.Fatal(err)
	}
	fixture.finish(t, job, "completed", nil)
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_find_fields_v1 WHERE event_hash=$1`, fields.EventHash); err != nil {
		t.Fatal(err)
	}
	fixture.read(t, fixture.repo, fields, domain.ErrNotFound)
	requeued := fixture.enqueue(t)
	if requeued.State != "waiting" || requeued.Version != job.Version+1 || requeued.Attempts != 0 || !requeued.LeaseUntil.IsZero() {
		t.Fatalf("completed demand did not requeue: %+v %+v", job, requeued)
	}
	if duplicate := fixture.enqueue(t); !reflect.DeepEqual(duplicate, requeued) {
		t.Fatalf("rebuild demand not idempotent while active: %+v %+v", duplicate, requeued)
	}
	rebuilt := fixture.claim(t)
	if err := fixture.storage.PutDocFindFields(fixture.ctx, rebuilt, fields); err != nil {
		t.Fatal(err)
	}
	fixture.finish(t, rebuilt, "completed", nil)
	fixture.read(t, fixture.repo, fields, nil)
}

func TestDocFindFieldsPGRetryReceiptPreservesCurrentBinding(t *testing.T) {
	fixture := docFindFieldsPG(t)
	fixture.enqueue(t)
	job := fixture.claim(t)
	fixture.finish(t, job, "retrying", nil)
	receipt := fixture.enqueue(t)
	if receipt.State != "retrying" || receipt.Version != job.Version || receipt.Attempts != job.Attempts {
		t.Fatalf("current source retry was reset: %+v %+v", job, receipt)
	}
	claimed := fixture.claim(t)
	fixture.changeSource(t)
	fixture.finish(t, claimed, "retrying", nil)
	refreshed := fixture.enqueue(t)
	if refreshed.State != "waiting" || refreshed.Version != claimed.Version+1 || refreshed.Attempts != 0 {
		t.Fatalf("retry source binding was not refreshed: %+v %+v", claimed, refreshed)
	}
}

func TestDocFindFieldsPGSourceAdmissionAndReadOnlyQueue(t *testing.T) {
	for _, missing := range []string{"snapshot", "ownership", "root_identity", "range_generation"} {
		t.Run(missing, func(t *testing.T) {
			fixture := docFindFieldsPG(t)
			statement := ""
			want := domain.ErrNotFound
			switch missing {
			case "snapshot":
				statement = `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`
			case "ownership":
				statement = `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='doc'`
			case "root_identity":
				statement = `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`
			case "range_generation":
				statement = `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$2 AND $1::text<>''`
				want = domain.ErrDocRangeIndexUnavailable
			}
			if _, err := fixture.storage.pool.Exec(fixture.ctx, statement, fixture.repo, fixture.doc); err != nil {
				t.Fatal(err)
			}
			if missing == "root_identity" {
				if err := fixture.storage.PutSnapshot(fixture.ctx, domain.Snapshot{ID: fixture.doc, RepoID: fixture.repo,
					DocHash: fixture.doc, DocIdentity: domain.DocumentIdentityLegacy, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
					t.Fatal(err)
				}
			}
			if job, err := fixture.storage.EnqueueDocFindFields(fixture.ctx, fixture.repo, fixture.doc, time.Now().UTC()); !errors.Is(err, want) || job != (domain.DocRangeIndexJob{}) {
				t.Fatalf("admission %s: %+v %v, want %v", missing, job, err, want)
			}
		})
	}
	fixture := docFindFieldsPG(t)
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		_, err := fixture.storage.EnqueueDocFindFields(read, fixture.repo, fixture.doc, time.Now().UTC())
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("enqueue inside borrowed read: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fixture.enqueue(t)
}

func TestDocFindFieldsPGBoundedQueueAndEnqueueLock(t *testing.T) {
	fixture := docFindFieldsPG(t)
	queued := fixture.enqueue(t)
	for index := 1; index < domain.MaxPendingDocRangeJobs; index++ {
		doc := domain.HashContent([]byte(fmt.Sprintf("%s:%d", t.Name(), index)))
		if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_find_fields_jobs_v1
 (repo_id,doc_hash,source_version,state,created_at,next_attempt) VALUES($1,$2,0,'waiting',$3,$3)`, fixture.repo, doc, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if duplicate := fixture.enqueue(t); !reflect.DeepEqual(duplicate, queued) {
		t.Fatalf("full queue lost idempotent receipt: %+v %+v", duplicate, queued)
	}
	fixture.changeSource(t)
	requeued := fixture.enqueue(t)
	if requeued.Version != queued.Version+1 {
		t.Fatalf("full queue could not replace stale active binding: %+v", requeued)
	}
	other := docRangeJobPGSource(t, fixture.storage, fixture.ctx, fixture.repo, 2)
	if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, other.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.EnqueueDocFindFields(fixture.ctx, fixture.repo, other.hash, time.Now().UTC()); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatalf("queue exceeded bound: %v", err)
	}
	blocker, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(blocker)
	if _, err := blocker.Exec(fixture.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "cxthub-doc-find-fields-enqueue:"+string(fixture.repo)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := fixture.storage.EnqueueDocFindFields(fixture.ctx, fixture.repo, fixture.doc, time.Now().UTC()); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatalf("unbounded enqueue lock: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("enqueue exceeded budget: %s", elapsed)
	}
}

func TestDocFindFieldsRejectsInvalidJobsWithoutDatabase(t *testing.T) {
	storage := &PostgresStore{}
	now := time.Now().UTC()
	job := domain.DocRangeIndexJob{RepoID: domain.HashContent([]byte("repo")), DocHash: domain.HashContent([]byte("doc")), State: "running", CreatedAt: now, NextAttempt: now, LeaseUntil: now.Add(time.Minute)}
	fields := docFindFieldsTestFields(t)
	job.BaseHash = domain.HashContent([]byte("base"))
	if err := storage.PutDocFindFields(context.Background(), job, fields); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("put accepted base hash: %v", err)
	}
	if err := storage.RenewDocFindFields(context.Background(), job, now, time.Minute); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("renew accepted base hash: %v", err)
	}
	job.State = "completed"
	if err := storage.FinishDocFindFields(context.Background(), job, now); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("finish accepted base hash: %v", err)
	}
	job.BaseHash, job.State = "", "running"
	read := outbound.WithDocReadOnly(context.Background())
	if err := storage.PutDocFindFields(read, job, fields); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("read-only context wrote sidecar: %v", err)
	}
	if _, err := storage.EnqueueDocFindFields(read, job.RepoID, job.DocHash, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("read-only context enqueued: %v", err)
	}
	if _, err := storage.ClaimDocFindFields(read, now, time.Minute); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("read-only context claimed: %v", err)
	}
	if err := storage.RenewDocFindFields(read, job, now, time.Minute); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("read-only context renewed: %v", err)
	}
	job.State = "completed"
	if err := storage.FinishDocFindFields(read, job, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("read-only context finished: %v", err)
	}
}
