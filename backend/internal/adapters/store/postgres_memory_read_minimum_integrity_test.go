//go:build postgres

package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGMemoryReadMinimumCorruptionOwnershipAndExpiry(test *testing.T) {
	for _, scenario := range []string{"qualification", "descriptor", "chunk", "chunk-hash", "version", "ownership", "incarnation", "expiry", "pruned", "scope"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
			identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
			requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
			expected := domain.ErrMemoryReadNotReady
			mayConsume := false
			switch scenario {
			case "qualification":
				expected = domain.ErrIntegrity
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_read_qualifications SET body=convert_to('{}','UTF8') WHERE repo_id=$1 AND qualification_id=$2`, string(job.Target.RepoID), string(identifier))
			case "descriptor":
				expected = domain.ErrIntegrity
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_read_headers SET header=convert_to('{}','UTF8') WHERE repo_id=$1 AND read_id=$2`, string(job.Target.RepoID), string(qualification.ReadID))
			case "chunk":
				expected, mayConsume = domain.ErrIntegrity, true
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_read_chunks SET body=convert_to('[]','UTF8') WHERE repo_id=$1 AND read_id=$2 AND ordinal=0`, string(job.Target.RepoID), string(qualification.ReadID))
			case "chunk-hash":
				expected, mayConsume = domain.ErrIntegrity, true
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_read_chunks SET body=convert_to(replace(convert_from(body,'UTF8'),'bounded result','altered result'),'UTF8') WHERE repo_id=$1 AND read_id=$2 AND ordinal=0`, string(job.Target.RepoID), string(qualification.ReadID))
			case "version":
				catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(qualification.Witnesses[0].Hash))
				if current := memorySourceGenerationPG(test, ctx, storage, job.Target.RepoID); current != requirement.Minimum {
					test.Fatal("physical-version mutation changed logical source")
				}
			case "ownership":
				catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory' AND hash=$2`, string(job.Target.RepoID), string(qualification.Witnesses[0].Hash))
			case "incarnation":
				memoryBundlePGFault(test, storage, ctx, `UPDATE blob_source_incarnations SET incarnation=gen_random_uuid() WHERE hash=$1`, string(qualification.Witnesses[0].Hash))
			case "expiry":
				memoryBundlePGExpire(test, storage, ctx, job.Target.RepoID, qualification.CaptureID)
			case "pruned":
				catalogExecPG(test, ctx, storage.pool, `DELETE FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(job.Target.RepoID), string(qualification.CaptureID))
			case "scope":
				expected = domain.ErrCatalogUnsupported
				if err := storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, false); err != nil {
					test.Fatal(err)
				}
			}
			for _, generation := range []domain.ContentHash{"", identifier} {
				wanted := expected
				if generation != "" && (scenario == "expiry" || scenario == "pruned") {
					wanted = domain.ErrMemoryCaptureExpired
				}
				called := false
				err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, generation, memoryQualificationAllowPG,
					func(bound context.Context, _ domain.MemoryReadStatus, _ domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
						called = true
						_, _ = reader.ReadChunk(bound, 0)
						return nil
					})
				if !errors.Is(err, wanted) || called != mayConsume {
					test.Fatal("invalid bounded read exposed result", generation, err, wanted, called)
				}
			}
		})
	}
}

func TestPGMemoryReadMinimumFreshAuthorizationAfterRevocation(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
	session := seedSessionMigration(test, storage)
	authorize := func(bound context.Context) (context.Context, error) {
		if _, err := storage.GetSession(bound, session.Token); err != nil {
			return nil, domain.ErrForbidden
		}
		return bound, nil
	}
	if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, identifier, authorize, memoryReadMinimumNoConsumePG); err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(stale context.Context) error {
		if _, err := storage.GetSession(stale, session.Token); err != nil {
			return err
		}
		if err := storage.DeleteSession(ctx, session.Token); err != nil {
			return err
		}
		for _, bound := range []context.Context{stale, ctx} {
			expected := domain.ErrForbidden
			if bound == stale {
				expected = domain.ErrConflict
			}
			if _, err := storage.ObserveMemoryReadRequirement(bound, job.Target, authorize); !errors.Is(err, expected) {
				test.Fatal("observation reused revoked authority", err)
			}
			if err := storage.WithMinimumMemoryReadRange(bound, job.Target, requirement, identifier, authorize, memoryReadMinimumNoConsumePG); !errors.Is(err, expected) {
				test.Fatal("continuation reused revoked authority", err)
			}
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestPGMemoryReadMinimumSnapshotAndFinalExpiry(test *testing.T) {
	for _, scenario := range []string{"advance-during-authorization", "prune-during-read", "pinned-expiry", "latest-expiry"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
			identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
			requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
			generation := identifier
			if scenario == "latest-expiry" {
				generation = ""
			}
			if strings.HasSuffix(scenario, "expiry") {
				memoryReadPGExpireSoon(test, storage, ctx, job.Target.RepoID, qualification.CaptureID)
			}
			authorize := func(bound context.Context) (context.Context, error) {
				if got := memorySourceGenerationPG(test, bound, storage, job.Target.RepoID); got != requirement.Minimum {
					test.Fatal("unexpected source in authorization")
				}
				if scenario == "advance-during-authorization" {
					catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='concurrent logical advance' WHERE repo_id=$1`, string(job.Target.RepoID))
				}
				return bound, nil
			}
			err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, generation, authorize,
				func(bound context.Context, status domain.MemoryReadStatus, _ domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
					if status.Observed != requirement.Minimum || status.Freshness != "current" {
						test.Fatal("source fractured authorization snapshot", status)
					}
					if scenario == "prune-during-read" {
						catalogExecPG(test, ctx, storage.pool, `DELETE FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(job.Target.RepoID), string(qualification.CaptureID))
					} else if strings.HasSuffix(scenario, "expiry") {
						memoryReadPGWaitExpiry(test, storage, ctx, job.Target.RepoID, qualification.CaptureID)
					}
					_, err := reader.ReadChunk(bound, 0)
					return err
				})
			var expected error
			if scenario == "pinned-expiry" {
				expected = domain.ErrMemoryCaptureExpired
			} else if scenario == "latest-expiry" {
				expected = domain.ErrMemoryReadNotReady
			}
			if !errors.Is(err, expected) {
				test.Fatal("snapshot or final expiry failed", err, expected)
			}
			if scenario == "prune-during-read" {
				if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, identifier, memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); !errors.Is(err, domain.ErrMemoryCaptureExpired) {
					test.Fatal("next page ignored pruning", err)
				}
			}
		})
	}
}

func TestPGMemoryReadMinimumUnavailableArrivalDoesNotHydratePast(test *testing.T) {
	storage, ctx, job, qualification, output := memoryReadWorkOutputPG(test)
	body := []byte("previously unavailable object " + string(job.Target.RepoID))
	absent := domain.HashContent(body)
	header := output.Header
	header.Dependencies = append(header.Dependencies, domain.MemoryReadDependency{Hash: absent})
	slices.SortFunc(header.Dependencies, func(left, right domain.MemoryReadDependency) int {
		return strings.Compare(string(left.Hash), string(right.Hash))
	})
	output = memoryReadPGSeal(test, ctx, header, nil)
	memoryReadPGWrite(test, storage, ctx, output)
	qualification.ReadID = output.ID
	memoryReadComparisonRecordPG(test, storage, ctx, job, output)
	qualification.Witnesses = append(qualification.Witnesses, domain.MemoryObjectWitness{RepoID: job.Target.RepoID, Hash: absent})
	slices.SortFunc(qualification.Witnesses, func(left, right domain.MemoryObjectWitness) int {
		return strings.Compare(string(left.Hash), string(right.Hash))
	})
	identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(absent), body)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2)`, string(job.Target.RepoID), string(absent))
	err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, identifier, memoryQualificationAllowPG,
		func(_ context.Context, status domain.MemoryReadStatus, actual domain.MemoryReadHeader, _ outbound.MemoryReadChunkReader) error {
			if status.Freshness != "catching_up" || status.Applied != qualification.Source || len(actual.Dependencies) != 2 {
				test.Fatal("arrival changed captured result", status)
			}
			for _, dependency := range actual.Dependencies {
				if dependency.Hash == absent && dependency.Available {
					test.Fatal("previously unavailable object hydrated into past")
				}
			}
			return nil
		})
	if err != nil {
		test.Fatal("arrival invalidated complete historical result", err)
	}
}

type memoryReadMinimumTracer struct {
	memoryReadRangeTracer
	hydrated atomic.Bool
}

func (tracer *memoryReadMinimumTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	query := strings.ToLower(data.SQL)
	for _, forbidden := range []string{"memory_capture_parts", "memory_capture_blocks", "from history_events", "body.bytes", "select bytes", "select *"} {
		if strings.Contains(query, forbidden) {
			tracer.hydrated.Store(true)
		}
	}
	if strings.Contains(query, "from snapshots") && !strings.Contains(query, "select exists(select 1 from snapshots where repo_id=$1 and id=$2)") {
		tracer.hydrated.Store(true)
	}
	return ctx
}

func TestPGMemoryReadMinimumBoundedCostAndRangeLimits(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	tracer := &memoryReadMinimumTracer{}
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
	requirement := memoryReadMinimumObservePG(test, reader, observed, job.Target)
	if tracer.hydrated.Load() || counts.queries.Load() > 8 || counts.rows.Load() > 8 {
		test.Fatal("observation hydrated source or exceeded indexed budget", counts.queries.Load(), counts.rows.Load())
	}
	counts.queries.Store(0)
	counts.rows.Store(0)
	if err := reader.WithMinimumMemoryReadRange(observed, job.Target, requirement, "", memoryQualificationAllowPG,
		func(bound context.Context, _ domain.MemoryReadStatus, _ domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
			_, err := chunks.ReadChunk(bound, 1)
			return err
		}); err != nil {
		test.Fatal(err)
	}
	if tracer.hydrated.Load() || counts.queries.Load() > 16 || counts.rows.Load() > 24 {
		test.Fatal("minimum read hydrated input or scanned", counts.queries.Load(), counts.rows.Load())
	}
	for _, scenario := range []string{"chunk-limit", "byte-limit", "consumer-error", "cancel"} {
		test.Run(scenario, func(test *testing.T) {
			bound, cancel := context.WithCancel(ctx)
			defer cancel()
			expected := domain.ErrMemoryProjectionLimit
			err := storage.WithMinimumMemoryReadRange(bound, job.Target, requirement, identifier, memoryQualificationAllowPG,
				func(readContext context.Context, _ domain.MemoryReadStatus, _ domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
					switch scenario {
					case "chunk-limit":
						for range memoryReadRangeChunkLimit {
							if _, err := chunks.ReadChunk(readContext, 2); err != nil {
								test.Fatal("premature chunk limit", err)
							}
						}
					case "byte-limit":
						chunks.(*memoryReadRange).bytes = memoryReadRangeByteLimit
					case "consumer-error":
						expected = domain.ErrNotFound
						return expected
					case "cancel":
						expected = context.Canceled
						cancel()
					}
					_, _ = chunks.ReadChunk(readContext, 0)
					return nil
				})
			if !errors.Is(err, expected) {
				test.Fatal("range validation or consumer error suppressed", err, expected)
			}
		})
	}
}
