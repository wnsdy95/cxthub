//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyNormalizationParserReusePGCounts struct {
	queries       int
	bodyArguments int
	bodyBytes     int
	objectInserts int
	hashReads     int
}

type legacyNormalizationParserReusePGTrace struct {
	mutex  sync.Mutex
	counts legacyNormalizationParserReusePGCounts
}

func (trace *legacyNormalizationParserReusePGTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	trace.mutex.Lock()
	defer trace.mutex.Unlock()
	trace.counts.queries++
	for _, argument := range query.Args {
		if body, ok := argument.([]byte); ok {
			trace.counts.bodyArguments++
			trace.counts.bodyBytes += len(body)
		}
	}
	if strings.HasPrefix(query.SQL, "INSERT INTO doc_legacy_byte_stage_checkpoint_objects_v1") {
		trace.counts.objectInserts++
	}
	if strings.Contains(query.SQL, "FROM doc_legacy_byte_stage_checkpoint_objects_v1") &&
		strings.Contains(query.SQL, "encode(sha256(bytes),'hex')") {
		trace.counts.hashReads++
	}
	return ctx
}

func (*legacyNormalizationParserReusePGTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (trace *legacyNormalizationParserReusePGTrace) snapshot() legacyNormalizationParserReusePGCounts {
	trace.mutex.Lock()
	defer trace.mutex.Unlock()
	return trace.counts
}

func legacyNormalizationParserReusePGStart(test *testing.T) (*PostgresStore, context.Context, legacyByteStagePGFixture, domain.DocRangeIndexJob, legacyByteCheckpoint) {
	test.Helper()
	setup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	test.Setenv("CXT_TEST_DSN", rootProjectionIsolatedDSN(test, setup))
	return legacyCheckpointPGStart(test)
}

func legacyNormalizationParserReusePGPeer(test *testing.T, storage *PostgresStore, ctx context.Context) (*PostgresStore, *legacyNormalizationParserReusePGTrace) {
	test.Helper()
	trace := &legacyNormalizationParserReusePGTrace{}
	config := storage.pool.Config()
	config.MaxConns, config.MinConns = 1, 0
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	return &PostgresStore{pool: pool}, trace
}

func TestLegacyBytePreparationConfirmationPGBoundedQueriesAndFreshSource(test *testing.T) {
	storage, ctx, fixture, job, state := legacyNormalizationParserReusePGStart(test)
	peer, trace := legacyNormalizationParserReusePGPeer(test, storage, ctx)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	for attempt := 1; attempt <= 2; attempt++ {
		if err := peer.confirmLegacyBytePreparation(ctx, job, state.stage); err != nil {
			test.Fatal("current preparation was not confirmed", err)
		}
		counts := trace.snapshot()
		if counts.queries != 6*attempt || counts.bodyArguments != 0 || counts.bodyBytes != 0 {
			test.Fatal("confirmation repeated lifetime queries or accessed source bodies", counts, attempt)
		}
	}
	if legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
		test.Fatal("confirmation changed publication state")
	}
	object := legacyCheckpointPGObject("bounded-current-checkpoint-read")
	if err := storage.stageLegacyNormalizationParserObject(ctx, job, state, object.hash, object.data); err != nil {
		test.Fatal(err)
	}
	before := trace.snapshot()
	body, err := peer.readLegacyNormalizationCheckpointObject(ctx, job, state, object.hash, len(object.data))
	if err != nil || !bytes.Equal(body, object.data) || trace.snapshot().queries-before.queries != 8 {
		test.Fatal("current checkpoint read repeated source/stage/job admission", trace.snapshot(), before, err)
	}
	legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, "source version")
	if err := peer.confirmLegacyBytePreparation(ctx, job, state.stage); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("confirmation reused source authority across transactions", err)
	}
}

func TestLegacyBytePreparationConfirmationPGRechecksAfterAuthorityLockWait(test *testing.T) {
	for _, change := range []string{"lease expiry", "job fence", "healthy renewal", "source version", "ownership"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
			peer, _ := legacyNormalizationParserReusePGPeer(test, storage, ctx)
			connection, err := peer.pool.Acquire(ctx)
			if err != nil {
				test.Fatal(err)
			}
			waiter := connection.Conn().PgConn().PID()
			connection.Release()
			observer, err := storage.pool.Acquire(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer observer.Release()
			holder, err := storage.pool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(holder)
			var present int
			var row pgx.Row
			if change == "source version" {
				row = holder.QueryRow(ctx, `SELECT 1 FROM blobs WHERE hash=$1 FOR UPDATE`, job.DocHash)
			} else if change == "ownership" {
				row = holder.QueryRow(ctx, `SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2 FOR UPDATE`, job.RepoID, job.DocHash)
			} else {
				row = holder.QueryRow(ctx, `SELECT 1 FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' FOR UPDATE`, job.RepoID, job.DocHash)
			}
			if err := row.Scan(&present); err != nil {
				test.Fatal(err)
			}
			work, cancel := context.WithCancel(ctx)
			completed, finished := make(chan error, 1), make(chan struct{})
			go func() {
				defer close(finished)
				completed <- peer.confirmLegacyBytePreparation(work, job, state.stage)
			}()
			defer func() { cancel(); rollbackPG(holder); <-finished }()
			authorityScopeWaitPG(test, ctx, observer.Conn(), holder.Conn().PgConn().PID(), waiter, "FOR UPDATE", completed)
			update := `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()+interval '1 hour' WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`
			if change == "lease expiry" {
				update = `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute' WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`
			} else if change == "job fence" {
				update = `UPDATE doc_range_jobs_v1 SET version=version+1,state='waiting',lease_until=NULL WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`
			} else if change == "source version" {
				update = `UPDATE blobs SET bytes=bytes WHERE hash=$2 AND EXISTS(SELECT 1 FROM repos WHERE id=$1)`
			} else if change == "ownership" {
				update = `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`
			}
			if _, err := holder.Exec(ctx, update, job.RepoID, job.DocHash); err != nil {
				test.Fatal(err)
			}
			if err := holder.Commit(ctx); err != nil {
				test.Fatal(err)
			}
			select {
			case err := <-completed:
				want := domain.ErrConflict
				if change == "ownership" {
					want = domain.ErrNotFound
				}
				if change == "healthy renewal" && err != nil || change != "healthy renewal" && !errors.Is(err, want) {
					test.Fatal("job-lock wait reused stale admission or rejected a valid renewal", change, err)
				}
			case <-ctx.Done():
				test.Fatal("confirmation did not finish after releasing the job lock", ctx.Err())
			}
		})
	}
}

func TestLegacyNormalizationParserReusePGHitSendsNoBodyAndTouchesCurrentSequence(test *testing.T) {
	storage, ctx, fixture, job, initial := legacyNormalizationParserReusePGStart(test)
	peer, trace := legacyNormalizationParserReusePGPeer(test, storage, ctx)
	body := bytes.Repeat([]byte{'p'}, domain.LegacyCanonicalEncodingBytes)
	hash := domain.HashContent(body)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	if err := peer.stageLegacyNormalizationParserObject(ctx, job, initial, hash, body); err != nil {
		test.Fatal("missing parser object was not staged", err)
	}
	first := trace.snapshot()
	if first.objectInserts != 1 || first.bodyArguments != 1 || first.bodyBytes != len(body) || first.hashReads != 2 {
		test.Fatal("miss did not use the bounded verified object insertion", first)
	}
	root := legacyCheckpointPGObject("parser-reuse-next-checkpoint")
	current, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, initial, root.hash, len(root.data), []legacyByteCheckpointObject{root})
	if err != nil {
		test.Fatal(err)
	}
	var sequence int64
	if err := storage.pool.QueryRow(ctx, `SELECT last_sequence FROM doc_legacy_checkpoint_staging_v1
 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`, job.DocHash, current.stage.incarnation, hash).Scan(&sequence); err != nil || sequence != initial.sequence {
		test.Fatal("fixture did not retain the earlier staging sequence", sequence, err)
	}
	usage := legacyByteUsagePGRead(test, storage, ctx)
	if err := peer.stageLegacyNormalizationParserObject(ctx, job, current, hash, body); err != nil {
		test.Fatal("identical parser object was not reusable", err)
	}
	second := trace.snapshot()
	if second.objectInserts != first.objectInserts || second.bodyArguments != first.bodyArguments ||
		second.bodyBytes != first.bodyBytes || second.hashReads != first.hashReads+1 {
		test.Fatal("reuse retransmitted a body or skipped fresh server-side hashing", first, second)
	}
	if err := storage.pool.QueryRow(ctx, `SELECT last_sequence FROM doc_legacy_checkpoint_staging_v1
 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`, job.DocHash, current.stage.incarnation, hash).Scan(&sequence); err != nil || sequence != current.sequence {
		test.Fatal("reused object was not protected from checkpoint reclamation", sequence, err)
	}
	actual, err := storage.readLegacyNormalizationCheckpointObject(ctx, job, current, hash, len(body))
	if err != nil || !bytes.Equal(actual, body) {
		test.Fatal("reused bytes changed", err)
	}
	if after := legacyByteUsagePGRead(test, storage, ctx); after != usage {
		test.Fatal("reuse duplicated object accounting", usage, after)
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("parser object staging changed public state")
	}
}

func TestLegacyNormalizationParserReusePGValidatesIncomingBodyBeforeSQL(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	object := legacyCheckpointPGObject("existing-parser-object")
	if err := storage.stageLegacyNormalizationParserObject(ctx, job, state, object.hash, object.data); err != nil {
		test.Fatal(err)
	}
	peer, trace := legacyNormalizationParserReusePGPeer(test, storage, ctx)
	usage := legacyByteUsagePGRead(test, storage, ctx)
	for _, fixture := range []struct {
		name string
		hash domain.ContentHash
		body []byte
		want error
	}{
		{name: "empty", hash: object.hash, want: domain.ErrValidation},
		{name: "oversized", hash: object.hash, body: bytes.Repeat([]byte{'x'}, domain.LegacyCanonicalEncodingBytes+1), want: domain.ErrValidation},
		{name: "malformed hash", hash: "invalid", body: object.data, want: domain.ErrValidation},
		{name: "wrong hash on hit", hash: object.hash, body: []byte("different incoming bytes"), want: domain.ErrIntegrity},
		{name: "wrong hash on miss", hash: domain.HashContent([]byte("absent")), body: object.data, want: domain.ErrIntegrity},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			if err := peer.stageLegacyNormalizationParserObject(ctx, job, state, fixture.hash, fixture.body); !errors.Is(err, fixture.want) {
				test.Fatal("unverified incoming object reached reuse", err, fixture.want)
			}
		})
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := peer.stageLegacyNormalizationParserObject(canceled, job, state, object.hash, object.data); !errors.Is(err, context.Canceled) {
		test.Fatal("canceled validation was not preserved", err)
	}
	if queries := trace.snapshot(); queries.queries != 0 {
		test.Fatal("invalid incoming objects opened database transactions", queries)
	}
	if after := legacyByteUsagePGRead(test, storage, ctx); after != usage {
		test.Fatal("invalid incoming objects changed staging accounting", usage, after)
	}
}

func legacyNormalizationParserReusePGDamage(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob, state legacyByteCheckpoint, hash domain.ContentHash, body []byte) {
	test.Helper()
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	rows, err := transaction.Query(ctx, `SELECT conname,pg_get_constraintdef(oid) FROM pg_constraint
 WHERE conrelid='doc_legacy_byte_stage_checkpoint_objects_v1'::regclass AND contype='c' ORDER BY conname`)
	if err != nil {
		test.Fatal(err)
	}
	var constraints []struct{ name, definition string }
	for rows.Next() {
		var constraint struct{ name, definition string }
		if err := rows.Scan(&constraint.name, &constraint.definition); err != nil {
			rows.Close()
			test.Fatal(err)
		}
		constraints = append(constraints, constraint)
	}
	rows.Close()
	if rows.Err() != nil || len(constraints) == 0 {
		test.Fatal("owned checkpoint constraints were not found", rows.Err())
	}
	for _, constraint := range constraints {
		if _, err := transaction.Exec(ctx, `ALTER TABLE doc_legacy_byte_stage_checkpoint_objects_v1 DROP CONSTRAINT `+pgx.Identifier{constraint.name}.Sanitize()); err != nil {
			test.Fatal(err)
		}
	}
	if _, err := transaction.Exec(ctx, `ALTER TABLE doc_legacy_byte_stage_checkpoint_objects_v1 DISABLE TRIGGER guard_legacy_checkpoint_objects`); err != nil {
		test.Fatal(err)
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stage_checkpoint_objects_v1 SET bytes=$4
 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`, job.DocHash, state.stage.incarnation, hash, body)
	if err != nil || tag.RowsAffected() != 1 {
		test.Fatal("owned checkpoint corruption was not installed", tag.RowsAffected(), err)
	}
	if _, err := transaction.Exec(ctx, `ALTER TABLE doc_legacy_byte_stage_checkpoint_objects_v1 ENABLE TRIGGER guard_legacy_checkpoint_objects`); err != nil {
		test.Fatal(err)
	}
	for _, constraint := range constraints {
		if _, err := transaction.Exec(ctx, `ALTER TABLE doc_legacy_byte_stage_checkpoint_objects_v1 ADD CONSTRAINT `+pgx.Identifier{constraint.name}.Sanitize()+` `+constraint.definition+` NOT VALID`); err != nil {
			test.Fatal(err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
}

func TestLegacyNormalizationParserReusePGRejectsFreshStoredCorruptionWithoutOverwrite(test *testing.T) {
	for _, damage := range []string{"hash", "length"} {
		test.Run(damage, func(test *testing.T) {
			storage, ctx, fixture, job, initial := legacyNormalizationParserReusePGStart(test)
			object := legacyCheckpointPGObject("parser-object-checked-on-every-hit")
			if err := storage.stageLegacyNormalizationParserObject(ctx, job, initial, object.hash, object.data); err != nil {
				test.Fatal(err)
			}
			root := legacyCheckpointPGObject("current-root-with-an-older-parser-object")
			current, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, initial, root.hash, len(root.data), []legacyByteCheckpointObject{root})
			if err != nil {
				test.Fatal(err)
			}
			corrupt := bytes.Clone(object.data)
			if damage == "hash" {
				corrupt[0] ^= 1
			} else {
				corrupt = corrupt[:len(corrupt)-1]
			}
			legacyNormalizationParserReusePGDamage(test, storage, ctx, job, current, object.hash, corrupt)
			peer, trace := legacyNormalizationParserReusePGPeer(test, storage, ctx)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			usage := legacyByteUsagePGRead(test, storage, ctx)
			if err := peer.stageLegacyNormalizationParserObject(ctx, job, current, object.hash, object.data); !errors.Is(err, domain.ErrPreparedDocRangeObject) {
				test.Fatal("stored corruption did not return the prepared-object integrity error", err)
			}
			if queries := trace.snapshot(); queries.bodyArguments != 0 || queries.objectInserts != 0 || queries.hashReads != 1 {
				test.Fatal("corrupt hit sent replacement bytes or skipped the server proof", queries)
			}
			var stored []byte
			if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM doc_legacy_byte_stage_checkpoint_objects_v1
 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`, job.DocHash, current.stage.incarnation, object.hash).Scan(&stored); err != nil || !bytes.Equal(stored, corrupt) {
				test.Fatal("reuse silently replaced corrupt immutable bytes", err)
			}
			var sequence int64
			if err := storage.pool.QueryRow(ctx, `SELECT last_sequence FROM doc_legacy_checkpoint_staging_v1
 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`, job.DocHash, current.stage.incarnation, object.hash).Scan(&sequence); err != nil || sequence != initial.sequence {
				test.Fatal("corrupt reuse acquired a fresh staging pin", sequence, err)
			}
			if after := legacyByteUsagePGRead(test, storage, ctx); after != usage {
				test.Fatal("corrupt hit changed checkpoint accounting", usage, after)
			}
			if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
				test.Fatal("corrupt hit changed public state")
			}
		})
	}
}

func TestLegacyNormalizationParserReusePGKeepsCurrentFences(test *testing.T) {
	for _, change := range []string{"read only", "nested", "source version", "source incarnation", "ownership", "lease expiry", "job fence", "checkpoint sequence", "stage incarnation", "declared plan"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx, fixture, job, state := legacyNormalizationParserReusePGStart(test)
			object := legacyCheckpointPGObject("fenced-parser-reuse")
			if err := storage.stageLegacyNormalizationParserObject(ctx, job, state, object.hash, object.data); err != nil {
				test.Fatal(err)
			}
			selected, want := ctx, domain.ErrConflict
			switch change {
			case "read only":
				selected = outbound.WithDocReadOnly(ctx)
			case "nested":
				selected = context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{})
			case "checkpoint sequence":
				root := legacyCheckpointPGObject("advanced-parser-reuse-root")
				if _, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, state, root.hash, len(root.data), []legacyByteCheckpointObject{root}); err != nil {
					test.Fatal(err)
				}
			case "stage incarnation":
				state.stage.incarnation = "00000000-0000-0000-0000-000000000000"
			case "declared plan":
				session, err := storage.prepareLegacyByteBackfill(ctx, job)
				if err != nil {
					test.Fatal(err)
				}
				session.stage.incarnation = state.stage.incarnation
				stage, err := storage.stageLegacyByteSession(ctx, job, session, 1)
				if err != nil || stage.plan == "" {
					test.Fatal("fixture did not declare a canonical plan", stage, err)
				}
			default:
				legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, change)
				if change == "ownership" {
					want = domain.ErrNotFound
				}
			}
			usage := legacyByteUsagePGRead(test, storage, ctx)
			if err := storage.stageLegacyNormalizationParserObject(selected, job, state, object.hash, object.data); !errors.Is(err, want) || errors.Is(err, domain.ErrPreparedDocRangeObject) {
				test.Fatal("parser reuse bypassed or misclassified a current fence", err, want)
			}
			if after := legacyByteUsagePGRead(test, storage, ctx); after != usage {
				test.Fatal("rejected reuse changed checkpoint accounting", usage, after)
			}
		})
	}
}

func TestLegacyNormalizationParserReusePGFinalFenceRollsBackTouch(test *testing.T) {
	storage, ctx, fixture, job, initial := legacyNormalizationParserReusePGStart(test)
	object := legacyCheckpointPGObject("parser-reuse-final-fence")
	if err := storage.stageLegacyNormalizationParserObject(ctx, job, initial, object.hash, object.data); err != nil {
		test.Fatal(err)
	}
	root := legacyCheckpointPGObject("parser-reuse-final-fence-root")
	current, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, initial, root.hash, len(root.data), []legacyByteCheckpointObject{root})
	if err != nil {
		test.Fatal(err)
	}
	name := pgx.Identifier{domain.NewID("parser_reuse_fence_")}.Sanitize()
	statement := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.doc_hash='%s' AND NEW.hash='%s' AND NEW.last_sequence>OLD.last_sequence THEN
 UPDATE doc_range_jobs_v1 SET state='waiting',version=version+1,lease_until=NULL WHERE repo_id='%s' AND doc_hash=NEW.doc_hash;
 END IF; RETURN NEW; END; $$;
 CREATE TRIGGER %s AFTER UPDATE ON doc_legacy_checkpoint_staging_v1 FOR EACH ROW EXECUTE FUNCTION %s()`,
		name, job.DocHash, object.hash, job.RepoID, name, name)
	if _, err := storage.pool.Exec(ctx, statement); err != nil {
		test.Fatal(err)
	}
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	if err := storage.stageLegacyNormalizationParserObject(ctx, job, current, object.hash, object.data); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("reuse committed after losing its live claim", err)
	}
	var sequence int64
	if err := storage.pool.QueryRow(ctx, `SELECT last_sequence FROM doc_legacy_checkpoint_staging_v1
 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`, job.DocHash, current.stage.incarnation, object.hash).Scan(&sequence); err != nil || sequence != initial.sequence {
		test.Fatal("failed final fence leaked its staging touch", sequence, err)
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("failed final fence changed public state or its job")
	}
}
