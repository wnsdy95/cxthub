//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type rangeJobLifetimeLocksAccessTracePG struct {
	source atomic.Int64
	stage  atomic.Int64
}

func (trace *rangeJobLifetimeLocksAccessTracePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	statement := strings.ToLower(strings.Join(strings.Fields(query.SQL), " "))
	if strings.Contains(statement, "repo_blobs") || strings.Contains(statement, "from blobs") ||
		strings.Contains(statement, "join blobs") || strings.Contains(statement, "blob_source_incarnations") {
		trace.source.Add(1)
	}
	if strings.Contains(statement, "doc_legacy_byte_") {
		trace.stage.Add(1)
	}
	return ctx
}

func (*rangeJobLifetimeLocksAccessTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestPGRangeJobLifetimeBoundTransactionRetainsCascadeButAllowsCounter(test *testing.T) {
	fixture := newRangeJobLifetimeFixturePG(test)
	scope := fixture.scope
	storage, ctx := scope.storage, scope.ctx
	if fixture.claim.RepositoryEpoch == "" || fixture.claim.RepositoryEpoch != scope.epoch {
		test.Fatal("actual range claim did not capture the current repository epoch", fixture.claim)
	}
	holder := authorityScopePeerPG(test, storage, "", nil)
	writer := authorityScopePeerPG(test, storage, "", nil)
	observerStore := authorityScopePeerPG(test, storage, "", nil)
	observer, err := observerStore.pool.Acquire(ctx)
	if err != nil {
		test.Fatal("acquire the independent lifetime lock observer", err)
	}
	defer observer.Release()
	outer, err := holder.beginBoundDocRangeJob(ctx, fixture.claim)
	if err != nil {
		test.Fatal("begin the actual current range claim's repository-bound transaction", err)
	}
	defer rollbackPG(outer)
	holderPID := outer.Conn().PgConn().PID()
	writerPID := authorityScopePIDPG(test, ctx, writer)
	observerPID := observer.Conn().PgConn().PID()
	if holderPID == writerPID || holderPID == observerPID || writerPID == observerPID {
		test.Fatal("range holder, counter/deletion writer, and observer must use independent PostgreSQL backends")
	}
	marker := domain.NewID("range_job_lifetime_lock_")
	if _, err := outer.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, marker); err != nil {
		test.Fatal("mark the actual bound outer transaction", err)
	}
	beforeCounter := rangeJobLifetimeImagePG(test, fixture)
	var head int64
	var epoch string
	if err := observer.QueryRow(ctx, `SELECT head_seq,epoch::text FROM repository_catalog_state WHERE repo_id=$1`, scope.repo.ID).Scan(&head, &epoch); err != nil {
		test.Fatal(err)
	}
	var nextHead int64
	var nextEpoch string
	counterContext, stopCounter := context.WithTimeout(ctx, 5*time.Second)
	counterErr := writer.pool.QueryRow(counterContext, `SELECT seq,epoch::text FROM cxt_catalog_generation($1)`, scope.repo.ID).Scan(&nextHead, &nextEpoch)
	stopCounter()
	if counterErr != nil {
		test.Fatal("ordinary catalog allocation must commit before the bound range transaction ends", counterErr)
	}
	if nextHead != head+1 || nextEpoch != epoch || epoch != fixture.claim.RepositoryEpoch {
		test.Fatal("ordinary counter allocation changed the retained lifetime or failed to advance exactly once", head, nextHead, epoch, nextEpoch)
	}
	var committedHead int64
	var committedEpoch string
	if err := observer.QueryRow(ctx, `SELECT head_seq,epoch::text FROM repository_catalog_state WHERE repo_id=$1`, scope.repo.ID).
		Scan(&committedHead, &committedEpoch); err != nil || committedHead != nextHead || committedEpoch != epoch {
		test.Fatal("independent observer must see counter progress before holder commit", committedHead, committedEpoch, err)
	}
	var released bool
	if err := observer.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, marker).Scan(&released); err != nil || released {
		test.Fatal("counter progress must not release the actual bound outer transaction", released, err)
	}
	afterCounter := rangeJobLifetimeImagePG(test, fixture)
	for index := 1; index < len(beforeCounter); index++ {
		if beforeCounter[index] != afterCounter[index] {
			test.Fatal("ordinary counter allocation changed a non-catalog durable range witness", index)
		}
	}
	beforeDelete := docJobLifetimeLocksImagePG(test, ctx, storage.db(ctx))
	test.Log("privileged owned-fixture transaction deletes its one snapshot before its repos row to satisfy the existing NO ACTION FK; this is not a public repository deletion API")
	work, cancel, completed := authorityScopeRunPG(test, ctx, func(work context.Context) error {
		transaction, err := writer.pool.Begin(work)
		if err != nil {
			return err
		}
		defer rollbackPG(transaction)
		deleted, err := transaction.Exec(work, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`, scope.repo.ID, fixture.snapshot.ID)
		if err != nil {
			return err
		}
		if deleted.RowsAffected() != 1 {
			return fmt.Errorf("owned-fixture snapshot deletion affected %d rows, want one", deleted.RowsAffected())
		}
		deleted, err = transaction.Exec(work, `DELETE FROM repos WHERE id=$1`, scope.repo.ID)
		if err != nil {
			return err
		}
		if deleted.RowsAffected() != 1 {
			return fmt.Errorf("owned-fixture repos deletion affected %d rows, want one", deleted.RowsAffected())
		}
		return transaction.Commit(work)
	})
	defer cancel()
	authorityScopeWaitPG(test, work, observer.Conn(), holderPID, writerPID, "DELETE FROM repos WHERE id=$1", completed)
	if after := docJobLifetimeLocksImagePG(test, ctx, storage.db(ctx)); after != beforeDelete {
		test.Fatal("blocked owned-fixture deletion changed durable state before bound outer commit")
	}
	authorityScopeWaitPG(test, work, observer.Conn(), holderPID, writerPID, "DELETE FROM repos WHERE id=$1", completed)
	if err := outer.Commit(ctx); err != nil {
		test.Fatal("release the actual range lifetime blocker only at outer commit", err)
	}
	authorityScopeDonePG(test, work, completed)
	if err := observer.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, marker).Scan(&released); err != nil || !released {
		test.Fatal("outer commit did not release its transaction-scoped advisory witness", released, err)
	}
	var remaining [5]int
	if err := observer.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM repos WHERE id=$1),
 (SELECT count(*) FROM repository_catalog_state WHERE repo_id=$1),
 (SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1),
 (SELECT count(*) FROM repo_blobs WHERE repo_id=$1),
 (SELECT count(*) FROM snapshots WHERE repo_id=$1)`, scope.repo.ID).
		Scan(&remaining[0], &remaining[1], &remaining[2], &remaining[3], &remaining[4]); err != nil || remaining != [5]int{} {
		test.Fatal("owned-fixture deletion did not commit its expected cascade after outer commit", remaining, err)
	}
	assertRootRepoCascadeSurvivorPG(test, scope)
}

func TestPGRangeJobLifetimeStaleEpochPrecedesSourceAndStageAccess(test *testing.T) {
	fixture := newRangeJobLifetimeFixturePG(test)
	scope := fixture.scope
	storage, ctx := scope.storage, scope.ctx
	if fixture.claim.RepositoryEpoch == "" || fixture.claim.RepositoryEpoch != scope.epoch {
		test.Fatal("retain an actual original range claim, not a fabricated queue receipt", fixture.claim)
	}
	session, err := storage.prepareLegacyByteBackfill(ctx, fixture.claim)
	if err != nil || !session.plan.Valid() {
		test.Fatal("prepare the original live source session before owned-fixture cascade/recreation", err)
	}
	current, epoch := recreateRangeJobLifetimePG(test, fixture)
	if current.RepositoryEpoch != epoch || current.RepositoryEpoch == fixture.claim.RepositoryEpoch {
		test.Fatal("actual replacement claim must capture only the new repository lifetime", current, fixture.claim)
	}
	trace := &rangeJobLifetimeLocksAccessTracePG{}
	peer := authorityScopePeerPG(test, storage, "", trace)
	peer.ConfigureLegacyByteRangePublication(true)
	peer.ConfigureLegacyDerivedByteRangePublication(true)
	before := docJobLifetimeLocksImagePG(test, ctx, storage.db(ctx))
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"prepare-source", func() error {
			stale, err := peer.prepareLegacyByteBackfill(ctx, fixture.claim)
			if !reflect.ValueOf(stale).IsZero() {
				test.Error("stale epoch returned a source session")
			}
			return err
		}},
		{"stage-original-session", func() error {
			stage, err := peer.stageLegacyByteSession(ctx, fixture.claim, session, 1)
			if stage != (legacyByteStage{}) {
				test.Error("stale epoch returned byte-stage progress", stage)
			}
			return err
		}},
	} {
		if !fixture.claim.LeaseUntil.After(time.Now()) || !current.LeaseUntil.After(time.Now()) {
			test.Fatal("epoch-only rejection must be tested while both actual leases remain live", operation.name)
		}
		failure := operation.run()
		if !errors.Is(failure, domain.ErrConflict) {
			test.Errorf("%s with the original epoch returned %v, want ErrConflict before source/stage access", operation.name, failure)
		}
		if source, stage := trace.source.Load(), trace.stage.Load(); source != 0 || stage != 0 {
			test.Errorf("%s reached source/stage SQL before rejecting the stale epoch: source=%d stage=%d, error=%v", operation.name, source, stage, failure)
		}
		if after := docJobLifetimeLocksImagePG(test, ctx, storage.db(ctx)); after != before {
			test.Errorf("%s left durable effects in the owned database", operation.name)
		}
	}
	assertRootRepoCascadeSurvivorPG(test, scope)
	if test.Failed() {
		return
	}
	assertRangeJobLifetimeFreshCompletionPG(test, fixture, current, epoch)
}
