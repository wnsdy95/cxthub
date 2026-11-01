//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func legacyCheckpointPGStart(test *testing.T) (*PostgresStore, context.Context, legacyByteStagePGFixture, domain.DocRangeIndexJob, legacyByteCheckpoint) {
	test.Helper()
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	source, _, err := storage.readLegacyCaptureSource(ctx, job, nil, 0, 0)
	if err != nil {
		test.Fatal(err)
	}
	stage, err := storage.ensureLegacyNormalizationStage(ctx, job, source.stage)
	if err != nil || stage.incarnation == "" || stage.plan != "" || stage.complete || stage.next != 0 {
		test.Fatal("normalization stage was absent or declared a fake canonical plan", stage, err)
	}
	state, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	if err != nil || found || state.stage != stage || state.sequence != 0 || state.hash != "" || state.bytes != 0 {
		test.Fatal("new preparing stage fabricated a checkpoint", state, found, err)
	}
	return storage, ctx, fixture, job, state
}

func legacyCheckpointPGObject(value string) legacyByteCheckpointObject {
	data := []byte(value)
	return legacyByteCheckpointObject{hash: domain.HashContent(data), data: data}
}

func TestLegacyNormalizationCheckpointPGLifecycle(test *testing.T) {
	storage, ctx, fixture, job, initial := legacyCheckpointPGStart(test)
	root := legacyCheckpointPGObject("normalization-control:first")
	child := legacyCheckpointPGObject("immutable-spool-extent")
	if err := storage.stageLegacyNormalizationCheckpointObjects(ctx, job, initial, []legacyByteCheckpointObject{child}); err != nil {
		test.Fatal(err)
	}
	state, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, initial, root.hash, len(root.data), []legacyByteCheckpointObject{root, child})
	if err != nil || state.sequence != 1 || state.hash != root.hash || state.stage.incarnation != initial.stage.incarnation {
		test.Fatal("checkpoint root did not commit with its immutable objects", state, err)
	}
	usage := legacyByteUsagePGRead(test, storage, ctx)
	if usage.CheckpointObjects != 2 || usage.CheckpointBytes != int64(len(root.data)+len(child.data)) || usage.Objects != 0 || usage.Parts != 0 || usage.PayloadBytes != 0 || usage.SpecBytes != 0 {
		test.Fatal("checkpoint accounting was duplicated or mixed with canonical stream counters", usage)
	}
	if recovered, err := storage.newLegacyRecoveryWork(ctx, job); err != nil || recovered != nil {
		test.Fatal("normal Preparing stage was treated as corrupt/completed canonical recovery", recovered, err)
	}
	for _, object := range []legacyByteCheckpointObject{root, child} {
		actual, err := storage.readLegacyNormalizationCheckpointObject(ctx, job, state, object.hash, len(object.data))
		if err != nil || !bytes.Equal(actual, object.data) {
			test.Fatal("committed immutable checkpoint body was not readable", err)
		}
	}
	if _, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, initial, root.hash, len(root.data), []legacyByteCheckpointObject{root}); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("old CAS sequence advanced a committed root twice", err)
	}
	loaded, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	if err != nil || !found || loaded != state {
		test.Fatal("lost-response recovery could not observe the one committed root", loaded, found, err)
	}
	nextRoot := legacyCheckpointPGObject("normalization-control:second")
	next, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, state, nextRoot.hash, len(nextRoot.data), []legacyByteCheckpointObject{nextRoot})
	if err != nil || next.sequence != 2 {
		test.Fatal(err)
	}
	if err := storage.discardLegacyNormalizationCheckpoint(ctx, job, state); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("late corruption report deleted a newer checkpoint", err)
	}
	if _, err := storage.readLegacyNormalizationCheckpointObject(ctx, job, state, root.hash, len(root.data)); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("old snapshot continued reading through a newer root", err)
	}
	if err := storage.discardLegacyNormalizationCheckpoint(ctx, job, next); err != nil {
		test.Fatal(err)
	}
	cleared, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	usage = legacyByteUsagePGRead(test, storage, ctx)
	if err != nil || found || cleared.sequence != 3 || cleared.hash != "" || usage.CheckpointObjects != 0 || usage.CheckpointBytes != 0 || cleared.stage.incarnation != initial.stage.incarnation {
		test.Fatal("fenced discard did not release checkpoint pins/counters in its transaction", cleared, found, usage, err)
	}
	var original []byte
	if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, fixture.hash).Scan(&original); err != nil || !bytes.Equal(original, fixture.raw) {
		test.Fatal("checkpoint lifecycle rewrote archival conversation bytes", err)
	}
}

func TestLegacyNormalizationCheckpointPGDeclaresPlanOnce(test *testing.T) {
	storage, ctx, _, job, initial := legacyCheckpointPGStart(test)
	root := legacyCheckpointPGObject("frozen-post-decoder-state")
	state, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, initial, root.hash, len(root.data), []legacyByteCheckpointObject{root})
	if err != nil {
		test.Fatal(err)
	}
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	session.stage.incarnation = initial.stage.incarnation
	stage, err := storage.stageLegacyByteSession(ctx, job, session, 1)
	if err != nil || stage.incarnation != initial.stage.incarnation || stage.plan == "" {
		test.Fatal("canonical plan declaration replaced or corrupted the Preparing incarnation", stage, err)
	}
	loaded, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	if err != nil || !found || loaded.sequence != state.sequence || loaded.hash != state.hash || loaded.stage.plan != stage.plan {
		test.Fatal("canonical declaration lost the intermediate recovery root", loaded, found, err)
	}
	if _, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, state, root.hash, len(root.data), nil); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("normalization root was updated after its canonical plan was declared", err)
	}
	if err := storage.discardLegacyByteRecovery(ctx, job, stage); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("unversioned canonical cleanup deleted a pinned normalization checkpoint", err)
	}
}

func TestLegacyNormalizationCheckpointPGCASCompetition(test *testing.T) {
	storage, ctx, _, job, initial := legacyCheckpointPGStart(test)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, value := range []string{"first-contender", "second-contender"} {
		object := legacyCheckpointPGObject(value)
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, initial, object.hash, len(object.data), []legacyByteCheckpointObject{object})
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, domain.ErrConflict) {
			conflicts++
		} else {
			test.Fatal(err)
		}
	}
	state, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	if err != nil || wins != 1 || conflicts != 1 || !found || state.sequence != 1 {
		test.Fatal("concurrent checkpoint writers did not have one CAS winner", wins, conflicts, state, err)
	}
	usage := legacyByteUsagePGRead(test, storage, ctx)
	if usage.CheckpointObjects != 1 {
		test.Fatal("losing checkpoint transaction leaked its private object", usage)
	}
}

func TestLegacyNormalizationCheckpointPGFailedSaveRollsBack(test *testing.T) {
	storage, ctx, _, job, initial := legacyCheckpointPGStart(test)
	object := legacyCheckpointPGObject("must-rollback")
	missing := legacyCheckpointPGObject("missing-root")
	if _, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, initial, missing.hash, len(missing.data), []legacyByteCheckpointObject{object}); !errors.Is(err, domain.ErrPreparedDocRangeObject) {
		test.Fatal("missing root was committed", err)
	}
	actual, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	usage := legacyByteUsagePGRead(test, storage, ctx)
	if err != nil || found || actual != initial || usage.CheckpointBytes != 0 || usage.CheckpointObjects != 0 {
		test.Fatal("failed root commit leaked objects, sequence, or accounting", actual, usage, err)
	}
}

func TestLegacyNormalizationCheckpointPGCurrentFences(test *testing.T) {
	for _, change := range []string{"source version", "source incarnation", "ownership", "lease expiry", "job fence", "cancellation"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx, fixture, job, initial := legacyCheckpointPGStart(test)
			root := legacyCheckpointPGObject("current-source-bound-root")
			state, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, initial, root.hash, len(root.data), []legacyByteCheckpointObject{root})
			if err != nil {
				test.Fatal(err)
			}
			selected, cancel := context.WithCancel(ctx)
			defer cancel()
			want := domain.ErrConflict
			if change == "cancellation" {
				cancel()
				want = context.Canceled
			} else {
				legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, change)
				if change == "ownership" {
					want = domain.ErrNotFound
				}
			}
			if _, err := storage.saveLegacyNormalizationCheckpoint(selected, job, state, root.hash, len(root.data), nil); !errors.Is(err, want) {
				test.Fatal("write bypassed its current source/claim", err, want)
			}
			if _, err := storage.readLegacyNormalizationCheckpointObject(selected, job, state, root.hash, len(root.data)); !errors.Is(err, want) {
				test.Fatal("read bypassed its current source/claim", err, want)
			}
			if err := storage.discardLegacyNormalizationCheckpoint(selected, job, state); !errors.Is(err, want) {
				test.Fatal("discard bypassed its current source/claim", err, want)
			}
		})
	}
}

func TestLegacyNormalizationCheckpointPGPreparingGuards(test *testing.T) {
	storage, ctx, _, job, initial := legacyCheckpointPGStart(test)
	object := legacyCheckpointPGObject("guarded-root")
	for _, statement := range []string{
		`UPDATE doc_legacy_byte_stages_v1 SET checkpoint_sequence=1,checkpoint_bytes=1 WHERE doc_hash=$1`,
		`UPDATE doc_legacy_byte_stages_v1 SET checkpoint_sequence=2 WHERE doc_hash=$1`,
		`UPDATE doc_legacy_byte_stages_v1 SET header_version=1,header_bytes=2 WHERE doc_hash=$1`,
		`INSERT INTO doc_legacy_byte_stage_objects_v1(doc_hash,hash,bytes) VALUES($1,'sha256:'||encode(sha256('x'::bytea),'hex'),'x'::bytea)`,
		`UPDATE doc_legacy_byte_usage_v1 SET checkpoint_bytes=checkpoint_bytes+1 WHERE doc_hash=$1`,
	} {
		transaction, err := storage.pool.Begin(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if err := legacyByteCheckpointProtocols(ctx, transaction); err != nil {
			test.Fatal(err)
		}
		_, err = transaction.Exec(ctx, `SET LOCAL cxt.legacy_byte_header_protocol='1'`)
		if err == nil {
			_, err = transaction.Exec(ctx, statement, job.DocHash)
		}
		rollbackPG(transaction)
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "23514" {
			test.Fatal("Preparing/counter guard accepted an invalid transition", statement, err)
		}
	}
	state, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, initial, object.hash, len(object.data), []legacyByteCheckpointObject{object})
	if err != nil {
		test.Fatal(err)
	}
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	if err := legacyByteCheckpointProtocols(ctx, transaction); err != nil {
		test.Fatal(err)
	}
	_, err = transaction.Exec(ctx, `DELETE FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1`, job.DocHash)
	rollbackPG(transaction)
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.Code != "23514" {
		test.Fatal("live root did not pin its immutable checkpoint objects", err)
	}
	if _, err := storage.readLegacyNormalizationCheckpointObject(ctx, job, state, object.hash, len(object.data)); err != nil {
		test.Fatal(err)
	}
}

func TestLegacyNormalizationCheckpointPGNewClaimRetainsCommittedRoot(test *testing.T) {
	storage, ctx, fixture, job, initial := legacyCheckpointPGStart(test)
	root := legacyCheckpointPGObject("committed-before-worker-replacement")
	state, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, initial, root.hash, len(root.data), []legacyByteCheckpointObject{root})
	if err != nil {
		test.Fatal(err)
	}
	finished := job
	finished.State, finished.Reason, finished.NextAttempt = "retrying", "worker replacement", time.Now().UTC().Add(-time.Minute)
	if err := storage.FinishDocRangeJob(ctx, finished, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	current := docRangeJobPGClaim(test, storage, ctx, fixture.repo, fixture.hash)
	loaded, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, current)
	if err != nil || !found || loaded != state || current.Version == job.Version {
		test.Fatal("replacement claim lost its durable root or reused the old fence", loaded, current, err)
	}
	if body, err := storage.readLegacyNormalizationCheckpointObject(ctx, current, loaded, root.hash, len(root.data)); err != nil || !bytes.Equal(body, root.data) {
		test.Fatal("current claim could not read its predecessor's committed root", err)
	}
	if _, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, state, root.hash, len(root.data), nil); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("replaced worker could still commit", err)
	}
	if err := storage.discardLegacyNormalizationCheckpoint(ctx, job, state); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("replaced worker could discard the current worker's checkpoint", err)
	}
	if _, err := storage.saveLegacyNormalizationCheckpoint(ctx, current, loaded, root.hash, len(root.data), nil); err != nil {
		test.Fatal("replacement claim could not advance the committed root", err)
	}
}

func TestLegacyNormalizationCheckpointPGStageRecreationRejectsOldOwner(test *testing.T) {
	storage, ctx, _, job, initial := legacyCheckpointPGStart(test)
	root := legacyCheckpointPGObject("same-source-recreated-stage")
	old, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, initial, root.hash, len(root.data), []legacyByteCheckpointObject{root})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, job.DocHash); err != nil {
		test.Fatal(err)
	}
	source := old.stage
	source.incarnation = ""
	stage, err := storage.ensureLegacyNormalizationStage(ctx, job, source)
	if err != nil || stage.incarnation == old.stage.incarnation {
		test.Fatal("recreated stage reused its old incarnation", stage, err)
	}
	current, _, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	current, err = storage.saveLegacyNormalizationCheckpoint(ctx, job, current, root.hash, len(root.data), []legacyByteCheckpointObject{root})
	if err != nil || current.sequence != old.sequence || current.hash != old.hash {
		test.Fatal("fixture did not recreate the same root and sequence", current, err)
	}
	if _, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, old, root.hash, len(root.data), nil); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("old stage owner wrote through an identical root/sequence", err)
	}
	if err := storage.discardLegacyNormalizationCheckpoint(ctx, job, old); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("late old-stage cleanup removed the recreated checkpoint", err)
	}
	if body, err := storage.readLegacyNormalizationCheckpointObject(ctx, job, current, root.hash, len(root.data)); err != nil || !bytes.Equal(body, root.data) {
		test.Fatal("rejected old owner damaged current checkpoint", err)
	}
}

func TestLegacyNormalizationCheckpointPGFinalFenceRollsBackRootAndObjects(test *testing.T) {
	storage, ctx, fixture, job, initial := legacyCheckpointPGStart(test)
	before := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	name := pgx.Identifier{domain.NewID("checkpoint_final_fence_")}.Sanitize()
	statement := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.doc_hash='%s' AND NEW.checkpoint_sequence>OLD.checkpoint_sequence THEN
 UPDATE doc_range_jobs_v1 SET state='waiting',version=version+1,lease_until=NULL WHERE repo_id='%s' AND doc_hash=NEW.doc_hash;
 END IF; RETURN NEW; END; $$;
 CREATE TRIGGER %s AFTER UPDATE ON doc_legacy_byte_stages_v1 FOR EACH ROW EXECUTE FUNCTION %s()`, name, fixture.hash, fixture.repo, name, name)
	if _, err := storage.pool.Exec(ctx, statement); err != nil {
		test.Fatal(err)
	}
	defer func() {
		if _, err := storage.pool.Exec(context.WithoutCancel(ctx), `DROP TRIGGER IF EXISTS `+name+` ON doc_legacy_byte_stages_v1; DROP FUNCTION IF EXISTS `+name+`() `); err != nil {
			test.Error(err)
		}
	}()
	root := legacyCheckpointPGObject("must-not-commit-after-fence-change")
	if _, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, initial, root.hash, len(root.data), []legacyByteCheckpointObject{root}); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("checkpoint committed after losing its job fence", err)
	}
	actual, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	usage := legacyByteUsagePGRead(test, storage, ctx)
	if err != nil || found || actual != initial || usage.CheckpointBytes != 0 || usage.CheckpointObjects != 0 {
		test.Fatal("final fence failure leaked root, objects, or accounting", actual, usage, err)
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != before {
		test.Fatal("failed checkpoint transaction changed public state or its job")
	}
}

type legacyCheckpointPGCreationQueryKey struct{}

type legacyCheckpointPGCreationTrace struct {
	reads   atomic.Int32
	arrived chan struct{}
	release chan struct{}
}

func (trace *legacyCheckpointPGCreationTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	selected := strings.HasPrefix(query.SQL, "SELECT source_id::text") && strings.Contains(query.SQL, "checkpoint_sequence")
	return context.WithValue(ctx, legacyCheckpointPGCreationQueryKey{}, selected)
}

func (trace *legacyCheckpointPGCreationTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	selected, _ := ctx.Value(legacyCheckpointPGCreationQueryKey{}).(bool)
	if selected && trace.reads.Add(1) <= 2 {
		trace.arrived <- struct{}{}
		select {
		case <-trace.release:
		case <-ctx.Done():
		}
	}
}

func TestLegacyNormalizationCheckpointPGConcurrentInitialCreation(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	source, _, err := storage.readLegacyCaptureSource(ctx, job, nil, 0, 0)
	if err != nil {
		test.Fatal(err)
	}
	trace := &legacyCheckpointPGCreationTrace{arrived: make(chan struct{}, 2), release: make(chan struct{})}
	var release sync.Once
	config := storage.pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	defer release.Do(func() { close(trace.release) })
	peer := &PostgresStore{pool: pool}
	type outcome struct {
		stage legacyByteStage
		err   error
	}
	results := make(chan outcome, 2)
	for worker := 0; worker < 2; worker++ {
		go func() {
			stage, err := peer.ensureLegacyNormalizationStage(ctx, job, source.stage)
			results <- outcome{stage: stage, err: err}
		}()
	}
	for worker := 0; worker < 2; worker++ {
		select {
		case <-trace.arrived:
		case <-ctx.Done():
			test.Fatal("workers did not both observe the absent initial stage", ctx.Err())
		}
	}
	release.Do(func() { close(trace.release) })
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.stage.incarnation == "" || first.stage != second.stage {
		test.Fatal("initial creation was not idempotent under concurrent workers", first, second)
	}
}
