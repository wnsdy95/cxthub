//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestNormalizationCheckpointPGDeferredBatchFreshBoundary(test *testing.T) {
	for _, change := range []string{"current", "source version", "source incarnation", "ownership", "snapshot identity", "lease expiry", "job fence", "epoch", "stage incarnation", "sequence", "cancellation"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx, fixture, job, state := legacyNormalizationParserReusePGStart(test)
			peer, trace := normalizationBatchPGPeer(test, storage, ctx)
			work := &legacyNormalizationCheckpointWork{store: peer, job: job, state: state}
			selected, cancel := context.WithCancel(ctx)
			defer cancel()
			bound, batch, err := work.deferBatch(selected)
			if err != nil || batch.transaction != nil || trace.snapshot().queries != 0 {
				test.Fatal("deferred admission performed SQL or retained a transaction", err, trace.snapshot())
			}
			want := domain.ErrConflict
			switch change {
			case "current":
				want = nil
			case "cancellation":
				cancel()
				want = context.Canceled
			case "epoch":
				if _, err := storage.pool.Exec(ctx, `SELECT public.cxt_reset_catalog($1)`, job.RepoID); err != nil {
					test.Fatal(err)
				}
			case "stage incarnation":
				if _, err := storage.pool.Exec(ctx, `DELETE FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, job.DocHash); err != nil {
					test.Fatal(err)
				}
				source := state.stage
				source.incarnation = ""
				current, err := storage.ensureLegacyNormalizationStage(ctx, job, source)
				if err != nil || current.incarnation == state.stage.incarnation {
					test.Fatal("stage recreation did not change its incarnation", err)
				}
			case "sequence":
				root := legacyCheckpointPGObject("deferred-boundary-new-root")
				if _, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, state, root.hash, len(root.data), []legacyByteCheckpointObject{root}); err != nil {
					test.Fatal(err)
				}
			default:
				legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, change)
				if change == "ownership" || change == "snapshot identity" {
					want = domain.ErrNotFound
				}
			}
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			usage := legacyByteUsagePGRead(test, storage, ctx)
			if err := batch.finish(bound, nil); !errors.Is(err, want) || !batch.closed || batch.transaction != nil {
				test.Fatal("empty batch bypassed fresh authority or retained its owner", err, want)
			}
			if counts := trace.snapshot(); counts.bodyArguments != 0 || counts.objectInserts != 0 || counts.hashReads != 0 || change == "current" && counts.queries != 1 {
				test.Fatal("empty batch read bodies, mutated objects, or repeated admission", counts)
			}
			if public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) || usage != legacyByteUsagePGRead(test, storage, ctx) {
				test.Fatal("empty boundary changed private accounting or publication")
			}
			if _, err := work.currentBatch(context.WithValue(ctx, legacyNormalizationCheckpointBatchKey{}, batch)); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("finished empty batch retained authority", err)
			}
		})
	}
}

func TestNormalizationCheckpointPGDeferredBatchScopesAndExpectedState(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	peer, trace := normalizationBatchPGPeer(test, storage, ctx)
	work := &legacyNormalizationCheckpointWork{store: peer, job: job, state: state}
	for _, selected := range []context.Context{outbound.WithDocReadOnly(ctx), context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{})} {
		if _, batch, err := work.deferBatch(selected); !errors.Is(err, domain.ErrConflict) || batch != nil {
			test.Fatal("deferred batch borrowed forbidden authority", err)
		}
	}
	if trace.snapshot().queries != 0 {
		test.Fatal("forbidden contexts performed database work")
	}
	for _, epoch := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
		work.job.RepositoryEpoch = epoch
		if epoch != "not-a-uuid" {
			if _, err := storage.pool.Exec(ctx, `UPDATE repository_catalog_state SET epoch=$2::uuid WHERE repo_id=$1`, job.RepoID, epoch); err != nil {
				test.Fatal(err)
			}
		}
		if _, batch, err := work.deferBatch(ctx); !errors.Is(err, domain.ErrConflict) || batch != nil || trace.snapshot().queries != 0 {
			test.Fatal("deferred authority bypassed the original UUID/nonzero guard", epoch, err)
		}
	}
	work.job = job
	if _, err := storage.pool.Exec(ctx, `UPDATE repository_catalog_state SET epoch=$2::uuid WHERE repo_id=$1`, job.RepoID, job.RepositoryEpoch); err != nil {
		test.Fatal(err)
	}
	for _, change := range []string{"plan", "blocks", "next", "complete", "hash", "bytes", "local change", "foreign owner", "promotion failure"} {
		test.Run(change, func(test *testing.T) {
			work.state = state
			switch change {
			case "plan":
				work.state.stage.plan = domain.HashContent([]byte("unexpected plan"))
			case "blocks":
				work.state.stage.blocks++
			case "next":
				work.state.stage.next++
			case "complete":
				work.state.stage.complete = true
			case "hash":
				work.state.hash = domain.HashContent([]byte("unexpected root"))
			case "bytes":
				work.state.bytes++
			case "promotion failure":
				work.state.sequence++
			}
			bound, batch, err := work.deferBatch(ctx)
			if err != nil {
				test.Fatal(err)
			}
			if change == "foreign owner" {
				foreign := *work
				if _, err := foreign.currentBatch(bound); !errors.Is(err, domain.ErrConflict) || batch.transaction != nil {
					test.Fatal("deferred context admitted another owner", err)
				}
				work.state.sequence++
			} else if change == "local change" {
				work.state.sequence++
			} else if change == "promotion failure" {
				object := legacyCheckpointPGObject("stale-promotion")
				if err := work.stageParserObjects(bound, []legacyByteCheckpointObject{object}); !errors.Is(err, domain.ErrConflict) || !batch.closed || batch.transaction != nil {
					test.Fatal("failed promotion left reusable authority", err)
				}
			}
			if err := batch.finish(bound, nil); !errors.Is(err, domain.ErrConflict) || !batch.closed || batch.transaction != nil {
				test.Fatal("empty batch accepted changed expected state", change, err)
			}
		})
	}
}

func TestNormalizationCheckpointPGDeferredBatchIOStillProvesCurrentBytes(test *testing.T) {
	for _, operation := range []string{"read", "put"} {
		for _, damage := range []string{"hash", "length"} {
			test.Run(operation+"/"+damage, func(test *testing.T) {
				storage, ctx, fixture, job, state := legacyNormalizationParserReusePGStart(test)
				object := legacyCheckpointPGObject("deferred-io-must-prove-current-bytes")
				if err := storage.stageLegacyNormalizationParserObject(ctx, job, state, object.hash, object.data); err != nil {
					test.Fatal(err)
				}
				peer, trace := normalizationBatchPGPeer(test, storage, ctx)
				work := &legacyNormalizationCheckpointWork{store: peer, job: job, state: state}
				bound, batch, err := work.deferBatch(ctx)
				if err != nil {
					test.Fatal(err)
				}
				corrupt := bytes.Clone(object.data)
				if damage == "hash" {
					corrupt[0] ^= 1
				} else {
					corrupt = corrupt[:len(corrupt)-1]
				}
				legacyNormalizationParserReusePGDamage(test, storage, ctx, job, state, object.hash, corrupt)
				public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
				if operation == "read" {
					_, err = work.readParserObject(bound, object.hash, len(object.data))
				} else {
					err = work.stageParserObjects(bound, []legacyByteCheckpointObject{object})
				}
				if batch.transaction == nil {
					test.Fatal("first I/O did not acquire the original fenced transaction", err)
				}
				if err = batch.finish(bound, err); !errors.Is(err, domain.ErrPreparedDocRangeObject) || !batch.closed || batch.transaction != nil {
					test.Fatal("deferred I/O accepted corrupt bytes or retained authority", err)
				}
				counts := trace.snapshot()
				if counts.bodyArguments != 0 || counts.objectInserts != 0 || operation == "put" && counts.hashReads != 1 {
					test.Fatal("deferred reuse resent bytes or skipped hash proof", counts)
				}
				var actual []byte
				if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`, job.DocHash, state.stage.incarnation, object.hash).Scan(&actual); err != nil || !bytes.Equal(actual, corrupt) || public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) {
					test.Fatal("failed promotion rewrote corrupt or published state", err)
				}
			})
		}
	}
}

type normalizationDeferredProbeHookKey struct{}

type normalizationDeferredProbeTrace struct {
	normalizationBatchPGTrace
	afterProbe func()
}

func (trace *normalizationDeferredProbeTrace) TraceQueryStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx = trace.normalizationBatchPGTrace.TraceQueryStart(ctx, connection, data)
	if data.SQL == legacyNormalizationDeferredBatchCurrent && trace.afterProbe != nil {
		return context.WithValue(ctx, normalizationDeferredProbeHookKey{}, trace.afterProbe)
	}
	return ctx
}

func (*normalizationDeferredProbeTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if hook, ok := ctx.Value(normalizationDeferredProbeHookKey{}).(func()); ok {
		hook()
	}
}

func TestNormalizationCheckpointPGDeferredBatchPostQueryCancelClosesCapture(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	rangeSpoolTempDir(test)
	_, work, job, stored := normalizationParserRestartPGStart(test, storage, ctx, true)
	defer work.Close()
	normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
		return checkpoint.capture != nil && checkpoint.position == 14 && checkpoint.next.Parser != nil && checkpoint.next.State.Count > 0 && checkpoint.next.Parser.Input.Tree.DataBytes == len(stored)
	})
	selected, cancel := context.WithCancel(ctx)
	defer cancel()
	probed := false
	trace := &normalizationDeferredProbeTrace{afterProbe: func() { probed = true; cancel() }}
	config := work.store.pool.Config()
	config.MaxConns, config.MinConns, config.ConnConfig.Tracer = 1, 0, trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	peer := &PostgresStore{pool: pool}
	work.store, work.checkpoint.store = peer, peer
	for step := 0; step < normalizationCheckpointPGStepLimit; step++ {
		progress, err := work.Step(selected, job)
		if probed {
			if progress != 0 || !errors.Is(err, context.Canceled) || pool.Stat().AcquiredConns() != 0 {
				test.Fatal("post-query cancellation returned progress or retained a connection", progress, err)
			}
			if _, err := work.checkpoint.capture.Control(ctx); err == nil {
				test.Fatal("post-query cancellation left capture reusable")
			}
			if progress, err := work.Step(ctx, job); err == nil || progress == outbound.DocRangeWorkPublishable {
				test.Fatal("failed pure batch resumed its closed owner", progress, err)
			}
			return
		}
		if err != nil || progress != outbound.DocRangeWorkPending || work.work != nil {
			test.Fatal("existing parser fixture missed its pure batch boundary", step, progress, err)
		}
	}
	test.Fatal("existing parser fixture never reached a pure eight-quantum batch")
}
