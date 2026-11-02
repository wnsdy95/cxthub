//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyBytePrepareBoundaryContext struct {
	context.Context
	paused          atomic.Bool
	release         sync.Once
	entered, resume chan struct{}
}

func (ctx *legacyBytePrepareBoundaryContext) Err() error {
	if !ctx.paused.Load() {
		var callers [4]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		hash, _ := frames.Next()
		verifier, _ := frames.Next()
		if strings.HasSuffix(hash.Function, "domain.HashContentContext") &&
			strings.HasSuffix(verifier.Function, "store.(*PostgresStore).verifyLegacyMonolithicRangeSource") &&
			ctx.paused.CompareAndSwap(false, true) {
			close(ctx.entered)
			<-ctx.resume
		}
	}
	return ctx.Context.Err()
}

func (ctx *legacyBytePrepareBoundaryContext) unblock() {
	ctx.release.Do(func() { close(ctx.resume) })
}

func legacyBytePrepareBoundaryWrite(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture, change string) {
	test.Helper()
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `SET LOCAL lock_timeout='250ms'`); err != nil {
		test.Fatal(err)
	}
	execute := func(statement string, arguments ...any) {
		test.Helper()
		tag, err := transaction.Exec(ctx, statement, arguments...)
		if err != nil || tag.RowsAffected() != 1 {
			test.Fatalf("writer could not finish while original hashing was paused: change=%s rows=%d error=%v", change, tag.RowsAffected(), err)
		}
	}
	switch change {
	case "source version":
		execute(`UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash)
	case "ownership":
		execute(`DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, fixture.hash)
	case "lease expiry":
		execute(`UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, fixture.repo, fixture.hash)
	case "job fence":
		execute(`UPDATE doc_range_jobs_v1 SET state='waiting',version=version+1,lease_until=NULL
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, fixture.repo, fixture.hash)
	case "snapshot identity", "source incarnation":
		var snapshot string
		if err := transaction.QueryRow(ctx, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2 RETURNING to_jsonb(snapshots)::text`,
			fixture.repo, fixture.hash).Scan(&snapshot); err != nil {
			test.Fatal("snapshot writer blocked during original hashing", err)
		}
		if change == "snapshot identity" {
			execute(`INSERT INTO snapshots SELECT * FROM jsonb_populate_record(NULL::snapshots,
 jsonb_set($1::jsonb,'{doc_identity}',to_jsonb($2::text)))`, snapshot, domain.DocumentIdentityRootV1)
		} else {
			execute(`DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, fixture.hash)
			execute(`DELETE FROM blobs WHERE hash=$1`, fixture.hash)
			execute(`INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, fixture.hash, fixture.raw)
			execute(`INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, fixture.repo, fixture.hash)
			execute(`INSERT INTO snapshots SELECT * FROM jsonb_populate_record(NULL::snapshots,$1::jsonb)`, snapshot)
		}
	default:
		test.Fatal("unknown preparation boundary mutation", change)
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal("writer could not commit while original hashing was paused", err)
	}
}

func legacyBytePrepareBoundaryProof(test *testing.T, session legacyByteBackfillSession, fixture legacyByteStagePGFixture, source legacyByteStage) {
	test.Helper()
	want := domain.DocumentRef{Hash: fixture.hash, Identity: domain.DocumentIdentityLegacy}
	spec, err := domain.DecodeDocLegacyByteBackfillSpec(session.stage.plan, session.encoded)
	planned, planErr := session.plan.Spec()
	key, keyErr := session.plan.Key()
	if err != nil || planErr != nil || keyErr != nil || !session.plan.Valid() || spec != planned || key != session.stage.plan ||
		spec.Reference != want || spec.StreamBytes != int64(len(fixture.stream)) || spec.Blocks != session.stage.blocks ||
		session.repo != fixture.repo || session.hash != fixture.hash || session.stage.sourceID != source.sourceID ||
		session.stage.sourceVersion != source.sourceVersion || session.stage.next != 0 || session.stage.complete ||
		!session.proof.Valid() || session.proof.DocumentRef() != want || !bytes.Equal(session.proof.Bytes(), fixture.raw) {
		test.Fatalf("prepared session lost its exact original proof or physical binding: %v %v %v", err, planErr, keyErr)
	}
}

func TestLegacyBytePreparePGBoundaryReleasesReadBeforeHashAndRechecksAdmission(test *testing.T) {
	for _, change := range []string{"source version", "ownership", "healthy renewal", "unrelated work", "lease expiry", "job fence", "snapshot identity", "source incarnation", "cancellation"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 128<<10)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			var other legacyByteStagePGFixture
			var otherJob domain.DocRangeIndexJob
			if change == "unrelated work" {
				other = legacyByteStagePGSource(test, storage, ctx, 1<<10)
				otherJob = legacyByteStagePGClaim(test, storage, ctx, other)
			}
			var source legacyByteStage
			if err := storage.pool.QueryRow(ctx, `SELECT source.doc_range_source_version,incarnation.incarnation::text
 FROM blobs source JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash WHERE source.hash=$1`, fixture.hash).
				Scan(&source.sourceVersion, &source.sourceID); err != nil {
				test.Fatal(err)
			}
			private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			trace := &legacyByteStageSourceTrace{}
			config := storage.pool.Config()
			config.MaxConns, config.MinConns = 1, 0
			config.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(pool.Close)
			peer := &PostgresStore{pool: pool}
			work, cancel := context.WithCancel(ctx)
			paused := &legacyBytePrepareBoundaryContext{Context: work, entered: make(chan struct{}), resume: make(chan struct{})}
			type result struct {
				session legacyByteBackfillSession
				err     error
			}
			results, finished := make(chan result, 1), make(chan struct{})
			test.Cleanup(func() {
				cancel()
				paused.unblock()
				<-finished
			})
			go func() {
				defer close(finished)
				session, err := peer.prepareLegacyByteBackfill(paused, job)
				results <- result{session: session, err: err}
			}()
			select {
			case <-paused.entered:
			case <-finished:
				test.Fatalf("preparation never paused in the original hash: %v", (<-results).err)
			case <-ctx.Done():
				test.Fatal("original hash checkpoint was not reached", ctx.Err())
			}
			if trace.reads.Load() != 1 {
				test.Fatal("hash pause did not follow exactly one source-body SQL read", trace.reads.Load())
			}
			if legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
				test.Fatal("initial source read changed archival or job state")
			}
			writer, stop := context.WithTimeout(ctx, 2*time.Second)
			defer stop()
			switch change {
			case "healthy renewal":
				if err := storage.RenewDocRangeJob(writer, job, time.Now().UTC(), 3*time.Minute); err != nil {
					test.Fatal("healthy lease renewal blocked during original hashing", err)
				}
			case "unrelated work":
				session, err := storage.prepareLegacyByteBackfill(writer, otherJob)
				if err != nil || !session.proof.Valid() || !bytes.Equal(session.proof.Bytes(), other.raw) {
					test.Fatal("unrelated preparation could not finish during original hashing", err)
				}
			case "cancellation":
				cancel()
			default:
				legacyBytePrepareBoundaryWrite(test, storage, writer, fixture, change)
			}
			if acquired := pool.Stat().AcquiredConns(); acquired != 0 {
				test.Fatal("CPU-only original hashing retained an acquired database connection", acquired)
			}
			if change == "source version" || change == "source incarnation" {
				var current legacyByteStage
				if err := storage.pool.QueryRow(ctx, `SELECT source.doc_range_source_version,incarnation.incarnation::text
 FROM repo_blobs owned JOIN blobs source ON source.hash=owned.hash
 JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 WHERE owned.repo_id=$1 AND owned.kind='doc' AND owned.hash=$2`, fixture.repo, fixture.hash).
					Scan(&current.sourceVersion, &current.sourceID); err != nil ||
					(change == "source version" && (current.sourceVersion != source.sourceVersion+1 || current.sourceID != source.sourceID)) ||
					(change == "source incarnation" && (current.sourceVersion != source.sourceVersion || current.sourceID == source.sourceID)) {
					test.Fatal("writer did not preserve ownership and exercise the intended physical fence", current, source, err)
				}
			}
			public = legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			paused.unblock()
			var got result
			select {
			case got = <-results:
				<-finished
			case <-ctx.Done():
				test.Fatal("preparation did not finish after releasing original hashing", ctx.Err())
			}
			switch change {
			case "healthy renewal", "unrelated work":
				if got.err != nil {
					test.Fatal("unrelated activity invalidated the original proof", got.err)
				}
				legacyBytePrepareBoundaryProof(test, got.session, fixture, source)
			case "cancellation":
				if !errors.Is(got.err, context.Canceled) || !reflect.ValueOf(got.session).IsZero() {
					test.Fatal("canceled preparation returned a partial proof or wrong error", got.err)
				}
			default:
				if (!errors.Is(got.err, domain.ErrConflict) && !errors.Is(got.err, domain.ErrNotFound)) || !reflect.ValueOf(got.session).IsZero() {
					test.Fatal("changed admission returned a stale or partial original proof", got.err)
				}
			}
			if trace.reads.Load() != 1 || pool.Stat().AcquiredConns() != 0 {
				test.Fatal("preparation reread the source body or leaked a database connection", trace.reads.Load(), pool.Stat().AcquiredConns())
			}
			if legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private ||
				legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
				test.Fatal("preparation changed private stages, public publication, archival data, or the job")
			}
			if change == "cancellation" {
				session, err := peer.prepareLegacyByteBackfill(ctx, job)
				if err != nil {
					test.Fatal("canceled preparation did not release its transaction resources", err)
				}
				legacyBytePrepareBoundaryProof(test, session, fixture, source)
				if trace.reads.Load() != 2 || pool.Stat().AcquiredConns() != 0 {
					test.Fatal("fresh preparation after cancellation leaked or reread its original")
				}
			}
		})
	}
}
