//go:build postgres

package store

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestDocByteRequirementPGConcurrentPromotionRespectsQueueCap(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	storage.ConfigureLegacyByteRangePublication(true)
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	var first domain.ContentHash
	var prior domain.DocRangeIndexJob
	for position := 0; position < domain.MaxPendingDocRangeJobs; position++ {
		source := docRangeJobPGSource(test, storage, ctx, repo, position+1)
		docRangeJobPGDeclareLegacy(test, storage, ctx, repo, source.hash)
		queued := docRangeJobPGEnqueue(test, storage, ctx, repo, source.hash)
		if position == 0 {
			first, prior = source.hash, queued
		}
	}
	const count = 8
	receipts := make(chan domain.DocRangeIndexJob, count)
	failures := make(chan error, count)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range count {
		workers.Go(func() {
			<-start
			receipt, err := storage.EnqueueDocByteRangeJob(ctx, repo, first, time.Now().UTC())
			receipts <- receipt
			failures <- err
		})
	}
	close(start)
	workers.Wait()
	close(receipts)
	close(failures)
	for err := range failures {
		if err != nil {
			test.Fatal("promotion of existing work consumed an extra queue slot", err)
		}
	}
	current := docRangeJobPGGet(test, storage, ctx, repo, first)
	if current.Version != prior.Version+1 || current.State != "waiting" {
		test.Fatal("concurrent promotions produced multiple fences", prior, current)
	}
	for receipt := range receipts {
		if !reflect.DeepEqual(receipt, current) {
			test.Fatal("duplicate promotion returned different durable work", receipt, current)
		}
	}
	var minimum, queued int
	if err := storage.pool.QueryRow(ctx, `SELECT minimum_range_version FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2`, repo, first).Scan(&minimum); err != nil || minimum != 4 {
		test.Fatal("promotion lost the byte requirement", minimum, err)
	}
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1`, repo).Scan(&queued); err != nil || queued != domain.MaxPendingDocRangeJobs {
		test.Fatal("promotion exceeded the pending cap", queued, err)
	}
	extra := docRangeJobPGSource(test, storage, ctx, repo, domain.MaxPendingDocRangeJobs+1)
	docRangeJobPGDeclareLegacy(test, storage, ctx, repo, extra.hash)
	if _, err := storage.EnqueueDocByteRangeJob(ctx, repo, extra.hash, time.Now().UTC()); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("new byte work bypassed the pending cap", err)
	}
	if _, err := storage.EnqueueDocByteRangeJob(outbound.WithDocReadOnly(ctx), repo, first, time.Now().UTC()); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("read-only context mutated or enqueued work", err)
	}
	_, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET minimum_range_version=0,byte_source_version=NULL WHERE repo_id=$1 AND doc_hash=$2`, repo, first)
	var rejected *pgconn.PgError
	if !errors.As(err, &rejected) || rejected.Code != "23514" {
		test.Fatal("database permitted a byte requirement downgrade", err)
	}
	if after := docRangeJobPGGet(test, storage, ctx, repo, first); !reflect.DeepEqual(after, current) {
		test.Fatal("rejected requests changed the promoted job", after, current)
	}
}
