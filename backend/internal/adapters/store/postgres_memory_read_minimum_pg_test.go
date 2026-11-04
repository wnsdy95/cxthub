//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func memoryReadMinimumNoConsumePG(context.Context, domain.MemoryReadStatus, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
	return nil
}

func memoryReadMinimumObservePG(test *testing.T, storage *PostgresStore, ctx context.Context, target domain.MemoryReadTarget) domain.MemoryReadRequirement {
	test.Helper()
	requirement, err := storage.ObserveMemoryReadRequirement(ctx, target, memoryQualificationAllowPG)
	if err != nil {
		test.Fatal(err)
	}
	return requirement
}

func TestPGMemoryReadMinimumObserveCoherentAuthorizationAndNoEnqueue(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	before := memorySourceGenerationPG(test, ctx, storage, target.RepoID)
	authorized := false
	requirement, err := storage.ObserveMemoryReadRequirement(ctx, target, func(bound context.Context) (context.Context, error) {
		authorized = true
		transaction, ok := bound.Value(repositoryTxKey{}).(*repositoryTx)
		if !ok || transaction.owner != storage || transaction.repo != target.RepoID || !transaction.readOnly {
			test.Fatal("authorization did not receive repository snapshot")
		}
		var isolation, readOnly string
		if err := transaction.QueryRow(bound, `SELECT current_setting('transaction_isolation'),current_setting('transaction_read_only')`).Scan(&isolation, &readOnly); err != nil {
			test.Fatal(err)
		}
		if isolation != "repeatable read" || readOnly != "on" {
			test.Fatal("observer did not own repeatable-read snapshot", isolation, readOnly)
		}
		if got := memorySourceGenerationPG(test, bound, storage, target.RepoID); got != before {
			test.Fatal("authorization source changed")
		}
		catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='after authorization' WHERE repo_id=$1`, string(target.RepoID))
		memorySourceBlobPG(test, ctx, storage, target.RepoID, "memory", "after authorization")
		return bound, nil
	})
	if err != nil || !authorized || requirement.Minimum != before || requirement.Validate(target) != nil {
		test.Fatal("observer fractured authorized source", requirement, err)
	}
	after := memoryReadMinimumObservePG(test, storage, ctx, target)
	if after.Minimum.Graph.Sequence <= before.Graph.Sequence || after.Minimum.Sequence <= before.Sequence {
		test.Fatal("next observation did not acquire fresh source", before, after)
	}
	var workCount, readCount, captureCount int
	if err := storage.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM memory_read_work WHERE repo_id=$1),
 (SELECT count(*) FROM memory_read_headers WHERE repo_id=$1),
 (SELECT count(*) FROM memory_capture_headers WHERE repo_id=$1)`, string(target.RepoID)).Scan(&workCount, &readCount, &captureCount); err != nil {
		test.Fatal(err)
	}
	if workCount != 0 || readCount != 0 || captureCount != 0 {
		test.Fatal("observer enqueued or hydrated result", workCount, readCount, captureCount)
	}
	foreign := target
	foreign.RepoID = memorySourceRepoPG(test, ctx, storage, "foreign observer")
	if err := storage.ConfigureMemoryReadWork(ctx, foreign.RepoID, true); err != nil {
		test.Fatal(err)
	}
	if got, err := storage.ObserveMemoryReadRequirement(ctx, foreign, memoryQualificationAllowPG); !errors.Is(err, domain.ErrNotFound) || got != (domain.MemoryReadRequirement{}) {
		test.Fatal("observer accepted foreign normalized snapshot", got, err)
	}
}

func TestPGMemoryReadMinimumAuthorizationAndTransactionOwnership(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
	operations := []struct {
		name string
		call func(*PostgresStore, context.Context, func(context.Context) (context.Context, error)) error
	}{
		{"observe", func(candidate *PostgresStore, bound context.Context, authorize func(context.Context) (context.Context, error)) error {
			observed, err := candidate.ObserveMemoryReadRequirement(bound, job.Target, authorize)
			if err != nil && observed != (domain.MemoryReadRequirement{}) {
				test.Fatal("failed observation leaked requirement")
			}
			return err
		}},
		{"read", func(candidate *PostgresStore, bound context.Context, authorize func(context.Context) (context.Context, error)) error {
			return candidate.WithMinimumMemoryReadRange(bound, job.Target, requirement, identifier, authorize, memoryReadMinimumNoConsumePG)
		}},
	}
	for _, operation := range operations {
		test.Run(operation.name, func(test *testing.T) {
			for _, scenario := range []struct {
				name      string
				authorize func(context.Context) (context.Context, error)
				expected  error
			}{
				{"nil", nil, domain.ErrValidation},
				{"nil-context", func(context.Context) (context.Context, error) { return nil, nil }, domain.ErrConflict},
				{"lost-snapshot", func(context.Context) (context.Context, error) { return ctx, nil }, domain.ErrConflict},
				{"replaced-snapshot", func(bound context.Context) (context.Context, error) {
					prior := *bound.Value(repositoryTxKey{}).(*repositoryTx)
					return context.WithValue(bound, repositoryTxKey{}, &prior), nil
				}, domain.ErrConflict},
				{"replaced-transaction", func(bound context.Context) (context.Context, error) {
					bound.Value(repositoryTxKey{}).(*repositoryTx).Tx = nil
					return bound, nil
				}, domain.ErrConflict},
				{"denied", func(context.Context) (context.Context, error) { return nil, domain.ErrForbidden }, domain.ErrForbidden},
			} {
				if err := operation.call(storage, ctx, scenario.authorize); !errors.Is(err, scenario.expected) {
					test.Fatal(scenario.name, err)
				}
			}
			check := func(bound context.Context) error {
				for _, candidate := range []*PostgresStore{storage, {pool: storage.pool}} {
					if err := operation.call(candidate, bound, func(context.Context) (context.Context, error) {
						test.Fatal("borrowed transaction reached authorization")
						return nil, nil
					}); !errors.Is(err, domain.ErrConflict) {
						test.Fatal("borrowed transaction accepted", err)
					}
				}
				return nil
			}
			if err := storage.WithinReadSnapshot(ctx, check); err != nil {
				test.Fatal(err)
			}
			if err := storage.WithinRepository(ctx, job.Target.RepoID, check); err != nil {
				test.Fatal(err)
			}
			if err := check(context.WithValue(ctx, repositoryTxKey{}, (*repositoryTx)(nil))); err != nil {
				test.Fatal(err)
			}
			if err := operation.call(storage, outbound.WithDocReadOnly(ctx), memoryQualificationAllowPG); err != nil {
				test.Fatal("read-only marker prevented an owned read", err)
			}
		})
	}
	if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, identifier, memoryQualificationAllowPG, nil); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("nil consumer", err)
	}
	if err := storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, false); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM repository_memory_source_state WHERE repo_id=$1`, string(job.Target.RepoID))
	for _, operation := range operations {
		if err := operation.call(storage, ctx, func(context.Context) (context.Context, error) { return nil, domain.ErrForbidden }); !errors.Is(err, domain.ErrForbidden) {
			test.Fatal("availability or source checked before authorization", operation.name, err)
		}
		if err := operation.call(storage, ctx, memoryQualificationAllowPG); !errors.Is(err, domain.ErrCatalogUnsupported) {
			test.Fatal("disabled scope accepted", operation.name, err)
		}
	}
	if err := storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, true); err != nil {
		test.Fatal(err)
	}
	for _, operation := range operations {
		if err := operation.call(storage, ctx, func(context.Context) (context.Context, error) { return nil, domain.ErrForbidden }); !errors.Is(err, domain.ErrForbidden) {
			test.Fatal("broken source leaked before authorization", operation.name, err)
		}
	}
}

func TestPGMemoryReadMinimumFreshnessAndStrictRegression(test *testing.T) {
	storage, ctx, job, qualification, output := memoryReadWorkOutputPG(test)
	identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
	before := memoryReadWorkStatusPG(test, storage, ctx, job)
	for _, freshness := range []string{"current", "catching_up"} {
		if freshness == "catching_up" {
			catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='logical advance' WHERE repo_id=$1`, string(job.Target.RepoID))
			memorySourceBlobPG(test, ctx, storage, job.Target.RepoID, "memory", "unrelated later memory")
		}
		observed := memorySourceGenerationPG(test, ctx, storage, job.Target.RepoID)
		for _, generation := range []domain.ContentHash{"", identifier} {
			var escaped outbound.MemoryReadChunkReader
			var escapedContext context.Context
			err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, generation, memoryQualificationAllowPG,
				func(bound context.Context, status domain.MemoryReadStatus, header domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
					if !storage.InReadOnlyTransaction(bound) || !reflect.DeepEqual(header, output.Header) || status.Validate() != nil ||
						status.Minimum != requirement.Minimum || status.Applied != qualification.Source || status.Observed != observed ||
						status.Generation != identifier || status.Freshness != freshness || !status.Complete {
						test.Fatal("incorrect bounded metadata", status)
					}
					escaped, escapedContext = reader, bound
					items, err := reader.ReadChunk(bound, 1)
					if err != nil || len(items) != header.Chunks[1].Count {
						test.Fatal("selected chunk", len(items), err)
					}
					return nil
				})
			if err != nil {
				test.Fatal(freshness, generation, err)
			}
			if _, err := escaped.ReadChunk(escapedContext, 1); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("reader escaped fresh snapshot", err)
			}
		}
	}
	if err := storage.WithCurrentMemoryReadRange(ctx, job.Target, memoryQualificationAllowPG, memoryReadWorkNoConsumePG); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("strict current reader accepted stale source", err)
	}
	if err := storage.WithQualifiedMemoryReadRange(ctx, job.Target.RepoID, identifier, memoryQualificationAllowPG, memoryReadWorkNoConsumePG); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("strict qualified reader accepted stale source", err)
	}
	if after := memoryReadWorkStatusPG(test, storage, ctx, job); after != before {
		test.Fatal("bounded reader modified work")
	}
	newRequirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
	if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, newRequirement, identifier, memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("new request incorrectly inherited old minimum", err)
	}
}

func TestPGMemoryReadMinimumOrderResetAndTargetBinding(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
	for _, scenario := range []string{"graph-floor", "memory-floor", "incomparable-floor", "graph-reset", "memory-reset", "foreign-minimum", "foreign-target", "selection", "content", "invalid-version"} {
		test.Run(scenario, func(test *testing.T) {
			target, wanted := job.Target, requirement
			expected := domain.ErrMemoryReadNotReady
			switch scenario {
			case "graph-floor":
				wanted.Minimum.Graph.Sequence++
			case "memory-floor":
				wanted.Minimum.Sequence++
			case "incomparable-floor":
				wanted.Minimum.Graph.Sequence++
				wanted.Minimum.Sequence = 0
			case "graph-reset":
				wanted.Minimum.Graph.Epoch = "11111111-1111-4111-8111-111111111111"
				expected = domain.ErrCatalogResetRequired
			case "memory-reset":
				wanted.Minimum.Epoch = "11111111-1111-4111-8111-111111111111"
				expected = domain.ErrCatalogResetRequired
			case "foreign-minimum":
				wanted.Minimum.Graph.RepoID = domain.HashContent([]byte("foreign minimum"))
				expected = domain.ErrValidation
			case "foreign-target":
				wanted.TargetID = domain.HashContent([]byte("foreign target"))
				expected = domain.ErrValidation
			case "selection", "content":
				if scenario == "selection" {
					target.Selection.CodeCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				} else {
					target.Content = "claims"
				}
				var err error
				wanted.TargetID, err = domain.MemoryReadTargetID(ctx, target)
				if err != nil {
					test.Fatal(err)
				}
				expected = domain.ErrIntegrity
			case "invalid-version":
				wanted.Version++
				expected = domain.ErrValidation
			}
			called := false
			err := storage.WithMinimumMemoryReadRange(ctx, target, wanted, identifier, memoryQualificationAllowPG,
				func(context.Context, domain.MemoryReadStatus, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
					called = true
					return nil
				})
			if !errors.Is(err, expected) || called {
				test.Fatal("invalid floor or target exposed result", err, expected, called)
			}
		})
	}
	for _, scenario := range []string{"current-behind", "current-memory-reset", "current-graph-reset"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
			identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
			requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
			expected := domain.ErrCatalogResetRequired
			switch scenario {
			case "current-behind":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repository_memory_source_state SET head_seq=head_seq-1 WHERE repo_id=$1`, string(job.Target.RepoID))
				expected = domain.ErrMemoryReadNotReady
			case "current-memory-reset":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repository_memory_source_state SET epoch=gen_random_uuid() WHERE repo_id=$1`, string(job.Target.RepoID))
			case "current-graph-reset":
				catalogExecPG(test, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(job.Target.RepoID))
			}
			if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, identifier, memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); !errors.Is(err, expected) {
				test.Fatal("current source bound ignored", err, expected)
			}
		})
	}
}

func TestPGMemoryReadMinimumPinnedGenerationDoesNotFollowLatest(test *testing.T) {
	storage, ctx, job, qualification, output := memoryReadWorkOutputPG(test)
	requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
	alternateHeader := output.Header
	alternateHeader.Origin += "replacement"
	alternate := memoryReadPGSeal(test, ctx, alternateHeader, nil)
	memoryReadPGWrite(test, storage, ctx, alternate)
	memoryReadComparisonRecordPG(test, storage, ctx, job, alternate)
	identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	alternateQualification := qualification
	alternateQualification.ReadID = alternate.ID
	replacement, err := storage.QualifyMemoryRead(ctx, alternateQualification)
	if err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET qualification_id=$3 WHERE repo_id=$1 AND target_id=$2`, string(job.Target.RepoID), string(job.ID), string(replacement))
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='advance between pages' WHERE repo_id=$1`, string(job.Target.RepoID))
	read := func(generation, expected domain.ContentHash, expectedHeader domain.MemoryReadHeader) {
		test.Helper()
		err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, generation, memoryQualificationAllowPG,
			func(_ context.Context, status domain.MemoryReadStatus, header domain.MemoryReadHeader, _ outbound.MemoryReadChunkReader) error {
				if status.Generation != expected || !reflect.DeepEqual(header, expectedHeader) || status.Freshness != "catching_up" {
					test.Fatal("pinned generation followed mutable target pointer", status)
				}
				return nil
			})
		if err != nil {
			test.Fatal(err)
		}
	}
	read(identifier, identifier, output.Header)
	read("", replacement, alternate.Header)
	if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, domain.HashContent([]byte("missing generation")), memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); !errors.Is(err, domain.ErrMemoryCaptureExpired) {
		test.Fatal("missing pinned generation silently followed latest", err)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET source=convert_to('{}','UTF8') WHERE repo_id=$1 AND target_id=$2`, string(job.Target.RepoID), string(job.ID))
	read(identifier, identifier, output.Header)
	if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, "", memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("corrupt mutable work state ignored by latest lookup", err)
	}
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM memory_read_work WHERE repo_id=$1 AND target_id=$2`, string(job.Target.RepoID), string(job.ID))
	read(identifier, identifier, output.Header)
	if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, "", memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("missing work scanned for another qualification", err)
	}
	if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, domain.HashContent([]byte("missing generation")), memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); !errors.Is(err, domain.ErrMemoryCaptureExpired) {
		test.Fatal("missing pinned generation not explicitly expired", err)
	}
}

func TestPGMemoryReadMinimumMissingAndRejectedWork(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	requirement := memoryReadMinimumObservePG(test, storage, ctx, target)
	if err := storage.WithMinimumMemoryReadRange(ctx, target, requirement, "", memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("missing work", err)
	}
	job := memoryReadWorkRequestPG(test, storage, ctx, target)
	for _, state := range []string{"waiting", "retrying", "rejected"} {
		catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET state=$3 WHERE repo_id=$1 AND target_id=$2`, string(target.RepoID), string(job.ID), state)
		expected := domain.ErrMemoryReadNotReady
		if state == "rejected" {
			expected = domain.ErrMemoryReadNeedsAttention
		}
		before := memoryReadWorkStatusPG(test, storage, ctx, job)
		if err := storage.WithMinimumMemoryReadRange(ctx, target, requirement, "", memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); !errors.Is(err, expected) {
			test.Fatal(state, err)
		}
		if after := memoryReadWorkStatusPG(test, storage, ctx, job); after != before {
			test.Fatal("read scheduled preparation")
		}
	}
}
