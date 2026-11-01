//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGMemoryReadWorkCurrentAuthorizationRangeAndPriorPointer(test *testing.T) {
	storage, ctx, job, qualification, output := memoryReadWorkOutputPG(test)
	if err := storage.WithCurrentMemoryReadRange(ctx, job.Target, memoryQualificationAllowPG, memoryReadWorkNoConsumePG); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("uncompleted result exposed", err)
	}
	memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	var escaped outbound.MemoryReadChunkReader
	var escapedContext context.Context
	consume := func(bound context.Context, header domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
		if !storage.InReadOnlyTransaction(bound) || !reflect.DeepEqual(header, output.Header) {
			test.Fatal("current read did not bind snapshot and header")
		}
		escaped, escapedContext = reader, bound
		items, err := reader.ReadChunk(bound, 1)
		if err != nil || len(items) != output.Header.Chunks[1].Count {
			test.Fatal("selected chunk", err)
		}
		return err
	}
	if err := storage.WithCurrentMemoryReadRange(outbound.WithDocReadOnly(ctx), job.Target, memoryQualificationAllowPG, consume); err != nil {
		test.Fatal(err)
	}
	if _, err := escaped.ReadChunk(escapedContext, 1); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("escaped current reader", err)
	}
	for _, state := range []string{"waiting", "retrying", "rejected"} {
		catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET state=$3 WHERE repo_id=$1 AND target_id=$2`, string(job.Target.RepoID), string(job.ID), state)
		if err := storage.WithCurrentMemoryReadRange(ctx, job.Target, memoryQualificationAllowPG, consume); err != nil {
			test.Fatal("valid prior pointer hidden during work", state, err)
		}
	}
	if err := storage.WithCurrentMemoryReadRange(ctx, job.Target, memoryQualificationAllowPG, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
		return domain.ErrEffectiveMemoryCursorStale
	}); !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
		test.Fatal("cursor error changed", err)
	}
	if err := storage.WithCurrentMemoryReadRange(ctx, job.Target, memoryQualificationAllowPG, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
		return domain.ErrNotFound
	}); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("consumer error changed", err)
	}
	for _, authorize := range []func(context.Context) (context.Context, error){nil,
		func(context.Context) (context.Context, error) { return nil, nil },
		func(context.Context) (context.Context, error) { return ctx, nil },
		func(context.Context) (context.Context, error) { return nil, domain.ErrForbidden },
	} {
		called := false
		err := storage.WithCurrentMemoryReadRange(ctx, job.Target, authorize, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
			called = true
			return nil
		})
		if err == nil || called {
			test.Fatal("invalid authorization exposed a result", err)
		}
	}
	missing := job.Target
	missing.Content = "prompt"
	if err := storage.WithCurrentMemoryReadRange(ctx, missing, func(context.Context) (context.Context, error) {
		return nil, domain.ErrForbidden
	}, memoryReadWorkNoConsumePG); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("availability leaked before authorization", err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		for _, candidate := range []*PostgresStore{storage, {pool: storage.pool}} {
			if err := candidate.WithCurrentMemoryReadRange(bound, job.Target, memoryQualificationAllowPG, consume); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("current selector accepted borrowed snapshot", err)
			}
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestPGMemoryReadWorkCurrentFreshRevocation(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	session := seedSessionMigration(test, storage)
	authorize := func(bound context.Context) (context.Context, error) {
		if _, err := storage.GetSession(bound, session.Token); err != nil {
			return nil, domain.ErrForbidden
		}
		return bound, nil
	}
	if err := storage.WithinReadSnapshot(ctx, func(stale context.Context) error {
		if _, err := storage.GetSession(stale, session.Token); err != nil {
			return err
		}
		if err := storage.DeleteSession(ctx, session.Token); err != nil {
			return err
		}
		if err := storage.WithCurrentMemoryReadRange(stale, job.Target, authorize, memoryReadWorkNoConsumePG); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("stale authorization accepted", err)
		}
		if err := storage.WithCurrentMemoryReadRange(ctx, job.Target, authorize, memoryReadWorkNoConsumePG); !errors.Is(err, domain.ErrForbidden) {
			test.Fatal("completed revocation ignored", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestPGMemoryReadWorkCurrentSourceWitnessAndIntegrity(test *testing.T) {
	for _, scenario := range []string{"source", "witness", "expiry", "qualification-corrupt", "chunk-corrupt", "rejected", "target-mismatch", "scope-disabled"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
			identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
			expected := domain.ErrMemoryReadNotReady
			mayConsume := false
			switch scenario {
			case "source":
				catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='new current source' WHERE repo_id=$1`, string(job.Target.RepoID))
			case "witness":
				before := memorySourceGenerationPG(test, ctx, storage, job.Target.RepoID)
				catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(qualification.Witnesses[0].Hash))
				if after := memorySourceGenerationPG(test, ctx, storage, job.Target.RepoID); after != before {
					test.Fatal("physical witness test changed logical source")
				}
			case "expiry":
				memoryBundlePGExpire(test, storage, ctx, job.Target.RepoID, qualification.CaptureID)
			case "qualification-corrupt":
				expected = domain.ErrIntegrity
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_read_qualifications SET body=convert_to('{}','UTF8') WHERE repo_id=$1 AND qualification_id=$2`, string(job.Target.RepoID), string(identifier))
			case "chunk-corrupt":
				expected, mayConsume = domain.ErrIntegrity, true
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_read_chunks SET body=convert_to('[]','UTF8') WHERE repo_id=$1 AND read_id=$2 AND ordinal=0`, string(job.Target.RepoID), string(qualification.ReadID))
			case "rejected":
				expected = domain.ErrMemoryReadNeedsAttention
				catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET state='rejected',qualification_id=NULL,reason='invalid_source' WHERE repo_id=$1`, string(job.Target.RepoID))
			case "target-mismatch":
				expected = domain.ErrIntegrity
				target := job.Target
				target.Content = "claims"
				other := memoryReadWorkRequestPG(test, storage, ctx, target)
				catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET qualification_id=$3 WHERE repo_id=$1 AND target_id=$2`, string(target.RepoID), string(other.ID), string(identifier))
				job = other
			case "scope-disabled":
				if err := storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, false); err != nil {
					test.Fatal(err)
				}
			}
			before := memoryReadWorkStatusPG(test, storage, ctx, job)
			called := false
			err := storage.WithCurrentMemoryReadRange(ctx, job.Target, memoryQualificationAllowPG, func(bound context.Context, _ domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
				called = true
				_, err := reader.ReadChunk(bound, 0)
				return err
			})
			if !errors.Is(err, expected) || called != mayConsume {
				test.Fatal("current read accepted invalid result", scenario, err, called)
			}
			if after := memoryReadWorkStatusPG(test, storage, ctx, job); after != before {
				test.Fatal("read enqueued or changed work")
			}
			if scenario == "witness" || scenario == "expiry" || scenario == "source" {
				requeued := memoryReadWorkRequestPG(test, storage, ctx, job.Target)
				if requeued.State != "waiting" || requeued.Version <= job.Version || requeued.QualificationID != identifier {
					test.Fatal("stale current did not requeue and retain prior pointer", requeued)
				}
			}
		})
	}
}

func TestPGMemoryReadWorkCurrentSnapshotAndFinalExpiry(test *testing.T) {
	for _, scenario := range []string{"prune", "source", "expiry"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
			memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
			if scenario == "expiry" {
				memoryReadPGExpireSoon(test, storage, ctx, job.Target.RepoID, qualification.CaptureID)
			}
			err := storage.WithCurrentMemoryReadRange(ctx, job.Target, memoryQualificationAllowPG, func(bound context.Context, _ domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
				switch scenario {
				case "prune":
					catalogExecPG(test, ctx, storage.pool, `DELETE FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(job.Target.RepoID), string(qualification.CaptureID))
				case "source":
					catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='advance during read' WHERE repo_id=$1`, string(job.Target.RepoID))
				case "expiry":
					memoryReadPGWaitExpiry(test, storage, ctx, job.Target.RepoID, qualification.CaptureID)
				}
				_, err := reader.ReadChunk(bound, 0)
				return err
			})
			if scenario == "expiry" {
				if !errors.Is(err, domain.ErrMemoryReadNotReady) {
					test.Fatal("expiry ignored at callback end", err)
				}
			} else if err != nil {
				test.Fatal("fresh read snapshot fractured", err)
			}
			if err := storage.WithCurrentMemoryReadRange(ctx, job.Target, memoryQualificationAllowPG, memoryReadWorkNoConsumePG); !errors.Is(err, domain.ErrMemoryReadNotReady) {
				test.Fatal("subsequent read used stale snapshot", err)
			}
		})
	}
}

func TestPGMemoryReadWorkStatusCanonicalIntegrity(test *testing.T) {
	for _, scenario := range []string{"target", "source", "whitespace", "identity"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, target := memoryReadWorkPG(test)
			job := memoryReadWorkRequestPG(test, storage, ctx, target)
			switch scenario {
			case "target":
				catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET target=convert_to('{}','UTF8') WHERE repo_id=$1`, string(target.RepoID))
			case "source":
				catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET source=convert_to('{}','UTF8') WHERE repo_id=$1`, string(target.RepoID))
			case "whitespace":
				catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET target=target || convert_to(' ','UTF8') WHERE repo_id=$1`, string(target.RepoID))
			case "identity":
				job.ID = domain.HashContent([]byte("forged target"))
				catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET target_id=$2 WHERE repo_id=$1`, string(target.RepoID), string(job.ID))
			}
			if _, err := storage.ReadMemoryReadWork(ctx, target.RepoID, job.ID); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("corrupt status did not preserve integrity error", scenario, err)
			}
		})
	}
}

type memoryReadWorkCostTracer struct {
	memoryReadRangeTracer
	hydrated atomic.Bool
}

func (tracer *memoryReadWorkCostTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	query := strings.ToLower(data.SQL)
	for _, forbidden := range []string{"memory_capture_parts", "memory_capture_blocks", "from snapshots", "from history_events", "body.bytes"} {
		if strings.Contains(query, forbidden) {
			tracer.hydrated.Store(true)
		}
	}
	return ctx
}

func TestPGMemoryReadWorkCurrentIndexedAndNoCaptureHydration(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	for ordinal := 1; ordinal < domain.MaxMemoryReadTargets; ordinal++ {
		target := job.Target
		target.Selection.CodeCommit = fmt.Sprintf("%040x", ordinal+1)
		work := memoryReadWorkRequestPG(test, storage, ctx, target)
		catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET state='rejected',reason='invalid_source' WHERE repo_id=$1 AND target_id=$2`, string(target.RepoID), string(work.ID))
	}
	catalogExecPG(test, ctx, storage.pool, `ANALYZE memory_read_work`)
	tracer := &memoryReadWorkCostTracer{}
	config := storage.pool.Config()
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	reader := &PostgresStore{pool: pool}
	counts := &memoryReadRangeCounts{}
	observed := context.WithValue(ctx, memoryReadRangeTraceKey{}, counts)
	if err := reader.WithCurrentMemoryReadRange(observed, job.Target, memoryQualificationAllowPG,
		func(bound context.Context, _ domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
			_, err := chunks.ReadChunk(bound, 1)
			return err
		}); err != nil {
		test.Fatal(err)
	}
	if tracer.hydrated.Load() || counts.queries.Load() > 16 || counts.rows.Load() > 24 {
		test.Fatal("current read hydrated input or scanned", tracer.hydrated.Load(), counts.queries.Load(), counts.rows.Load())
	}
	var plan []byte
	if err := storage.pool.QueryRow(ctx, `EXPLAIN (FORMAT JSON) SELECT `+memoryReadWorkColumns+` FROM memory_read_work WHERE repo_id=$1 AND target_id=$2`,
		string(job.Target.RepoID), string(job.ID)).Scan(&plan); err != nil {
		test.Fatal(err)
	}
	if !bytes.Contains(plan, []byte(`"Index Name": "memory_read_work_pkey"`)) || bytes.Contains(plan, []byte(`"Node Type": "Seq Scan"`)) {
		test.Fatal("current target lookup not indexed", string(plan))
	}
	test.Logf("current range SELECTs=%d rows=%d; target lookup uses primary key; no input hydration", counts.queries.Load(), counts.rows.Load())
}
