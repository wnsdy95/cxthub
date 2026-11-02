//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func memoryReadComparisonPG(test *testing.T, ctx context.Context, job domain.MemoryReadWork, output domain.MemoryReadBundle) domain.MemoryReadComparison {
	test.Helper()
	var items []domain.EffectiveMemoryItem
	for ordinal, chunk := range output.Chunks {
		decoded, err := domain.DecodeMemoryReadChunk(ctx, output.Header, ordinal, chunk.Bytes)
		if err != nil {
			test.Fatal(err)
		}
		items = append(items, decoded...)
	}
	delivery, err := domain.EffectiveMemoryDeliveryStateHash(ctx, job.Target.RepoID, output.Header.Origin, output.Header.View, items)
	if err != nil {
		test.Fatal(err)
	}
	if delivery != output.Header.View.DeliveryStateHash {
		test.Fatal("fixture delivery digest does not match reconstructed chunks")
	}
	actual, err := domain.MemoryReadComparisonStateHash(ctx, job.Target.RepoID, output.Header.Origin, output.Header.View)
	if err != nil {
		test.Fatal(err)
	}
	comparison := domain.MemoryReadComparison{Version: domain.MemoryReadComparisonVersion, ReadID: output.ID,
		CaptureID: output.Header.CaptureID, TargetID: job.ID, Source: output.Header.Source,
		Expected: actual, Actual: actual}
	return comparison
}

func memoryReadComparisonJSONPG(test *testing.T, comparison domain.MemoryReadComparison) []byte {
	test.Helper()
	raw, err := json.Marshal(comparison)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func memoryReadComparisonRecordPG(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.MemoryReadWork, output domain.MemoryReadBundle) domain.MemoryReadComparison {
	test.Helper()
	comparison := memoryReadComparisonPG(test, ctx, job, output)
	if err := storage.RecordMemoryReadComparison(ctx, job, comparison); err != nil {
		test.Fatal(err)
	}
	return comparison
}

func memoryReadComparisonCountPG(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash) int {
	test.Helper()
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM memory_read_comparisons WHERE repo_id=$1`, string(repo)).Scan(&count); err != nil {
		test.Fatal(err)
	}
	return count
}

func TestPGMemoryReadComparisonMatchDuplicateConflictAndRetention(test *testing.T) {
	storage, ctx, job, qualification, output := memoryReadWorkOutputWithoutComparisonPG(test)
	before := memorySourceGenerationPG(test, ctx, storage, job.Target.RepoID)
	comparison := memoryReadComparisonRecordPG(test, storage, ctx, job, output)
	peer, err := NewPostgresStore(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	if err := peer.RecordMemoryReadComparison(ctx, job, comparison); err != nil {
		test.Fatal("duplicate after restart", err)
	}
	alternate := comparison
	alternate.Expected = domain.HashContent([]byte("different expected projection"))
	if err := storage.RecordMemoryReadComparison(ctx, job, alternate); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("immutable receipt replaced", err)
	}
	transaction := catalogTxPG(test, ctx, storage)
	stored, actual, err := readMemoryReadComparison(ctx, transaction, job.Target.RepoID, output.ID, false)
	raw, encodeErr := domain.MemoryReadComparisonBytes(ctx, comparison)
	if err != nil || encodeErr != nil || actual != comparison || !bytes.Equal(raw, stored) || memoryReadComparisonCountPG(test, storage, ctx, job.Target.RepoID) != 1 {
		test.Fatal("receipt changed", actual, err, encodeErr)
	}
	if err := transaction.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE memory_read_comparisons SET body=body WHERE repo_id=$1 AND read_id=$2`, string(job.Target.RepoID), string(output.ID)); err == nil {
		test.Fatal("SQL update bypassed immutability")
	}
	identifier, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification)
	if err != nil {
		test.Fatal(err)
	}
	requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
	for _, generation := range []domain.ContentHash{"", identifier} {
		if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, generation, memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); err != nil {
			test.Fatal("matched receipt did not serve", err)
		}
	}
	if after := memorySourceGenerationPG(test, ctx, storage, job.Target.RepoID); after != before {
		test.Fatal("comparison or publication bumped logical source")
	}
	memoryBundlePGExpire(test, storage, ctx, job.Target.RepoID, qualification.CaptureID)
	if count, err := storage.PruneMemoryCaptures(ctx, job.Target.RepoID, 1); err != nil || count != 1 {
		test.Fatal("retention cleanup", count, err)
	}
	if memoryReadComparisonCountPG(test, storage, ctx, job.Target.RepoID) != 0 {
		test.Fatal("comparison outlived result retention")
	}
}

func TestPGMemoryReadComparisonMissingMismatchAndStrictIsolation(test *testing.T) {
	for _, scenario := range []string{"missing", "mismatch"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, output := memoryReadWorkOutputWithoutComparisonPG(test)
			expectedFinish, expectedRead := domain.ErrMemoryReadNotReady, domain.ErrMemoryReadNotReady
			count := 0
			if scenario == "mismatch" {
				comparison := memoryReadComparisonPG(test, ctx, job, output)
				comparison.Expected = domain.HashContent([]byte("different reconstruction"))
				if err := storage.RecordMemoryReadComparison(ctx, job, comparison); err != nil {
					test.Fatal("mismatch diagnostics rejected", err)
				}
				expectedFinish, expectedRead, count = domain.ErrMemoryReadComparisonMismatch, domain.ErrMemoryReadNeedsAttention, 1
			}
			before := memoryReadWorkStatusPG(test, storage, ctx, job)
			if identifier, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification); !errors.Is(err, expectedFinish) || identifier != "" {
				test.Fatal("uncompared result published", identifier, err)
			}
			if after := memoryReadWorkStatusPG(test, storage, ctx, job); after != before || memoryQualificationCountPG(test, storage, ctx, job.Target.RepoID) != 0 {
				test.Fatal("failed gate changed work or qualification", after)
			}
			if memoryReadComparisonCountPG(test, storage, ctx, job.Target.RepoID) != count {
				test.Fatal("failed gate discarded diagnostic receipt")
			}
			identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
			if err := storage.WithQualifiedMemoryReadRange(ctx, job.Target.RepoID, identifier, memoryQualificationAllowPG, memoryReadWorkNoConsumePG); err != nil {
				test.Fatal("strict qualification contract changed", err)
			}
			requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
			for _, generation := range []domain.ContentHash{"", identifier} {
				called := false
				err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, generation, memoryQualificationAllowPG,
					func(context.Context, domain.MemoryReadStatus, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
						called = true
						return nil
					})
				if !errors.Is(err, expectedRead) || called {
					test.Fatal("minimum reader bypassed comparison gate", generation, err, called)
				}
			}
		})
	}
}

func TestPGMemoryReadComparisonRejectsBindingsAndFences(test *testing.T) {
	for _, scenario := range []string{"target", "header-target", "repo", "read", "capture", "source", "below-claim", "future", "graph-reset", "memory-reset", "fence", "job-source", "lease", "disabled", "reenabled", "capture-expired", "readonly", "cancelled"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, _, output := memoryReadWorkOutputWithoutComparisonPG(test)
			comparison := memoryReadComparisonPG(test, ctx, job, output)
			parent := ctx
			switch scenario {
			case "target":
				comparison.TargetID = domain.HashContent([]byte("other target"))
			case "header-target":
				target := job.Target
				target.Selection.CodeCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				memoryReadWorkRequestPG(test, storage, ctx, target)
				job = memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
				comparison.TargetID = job.ID
			case "repo":
				comparison.Source.Graph.RepoID = domain.HashContent([]byte("other repo"))
			case "read":
				comparison.ReadID = domain.HashContent([]byte("missing read"))
			case "capture":
				comparison.CaptureID = domain.HashContent([]byte("other capture"))
			case "source":
				catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='source advance' WHERE repo_id=$1`, string(job.Target.RepoID))
				comparison.Source = memorySourceGenerationPG(test, ctx, storage, job.Target.RepoID)
			case "below-claim":
				comparison.Source.Graph.Sequence--
			case "future":
				comparison.Source.Sequence++
			case "graph-reset":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repository_graph_source_state SET epoch=gen_random_uuid() WHERE repo_id=$1`, string(job.Target.RepoID))
			case "memory-reset":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repository_memory_source_state SET epoch=gen_random_uuid() WHERE repo_id=$1`, string(job.Target.RepoID))
			case "fence":
				job.Version++
			case "job-source":
				job.Source.Sequence = 0
			case "lease":
				memoryReadWorkExpirePG(test, storage, ctx, job)
			case "disabled", "reenabled":
				if err := storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, false); err != nil {
					test.Fatal(err)
				}
				if scenario == "reenabled" {
					if err := storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, true); err != nil {
						test.Fatal(err)
					}
				}
			case "capture-expired":
				memoryBundlePGExpire(test, storage, ctx, job.Target.RepoID, comparison.CaptureID)
			case "readonly":
				ctx = outbound.WithDocReadOnly(ctx)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			before := memoryReadWorkStatusPG(test, storage, parent, job)
			if err := storage.RecordMemoryReadComparison(ctx, job, comparison); err == nil || (scenario == "cancelled" && !errors.Is(err, context.Canceled)) {
				test.Fatal("invalid record accepted", err)
			}
			if after := memoryReadWorkStatusPG(test, storage, parent, job); after != before || memoryReadComparisonCountPG(test, storage, parent, job.Target.RepoID) != 0 {
				test.Fatal("invalid comparison changed persistent state")
			}
		})
	}
}

func TestPGMemoryReadComparisonOwnedWriteOnly(test *testing.T) {
	storage, ctx, job, _, output := memoryReadWorkOutputWithoutComparisonPG(test)
	comparison := memoryReadComparisonPG(test, ctx, job, output)
	check := func(bound context.Context) error {
		peer := &PostgresStore{pool: storage.pool}
		for _, candidate := range []*PostgresStore{storage, peer} {
			if err := candidate.RecordMemoryReadComparison(bound, job, comparison); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("borrowed transaction accepted", err)
			}
		}
		return nil
	}
	if err := storage.WithinRepository(ctx, job.Target.RepoID, check); err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(ctx, check); err != nil {
		test.Fatal(err)
	}
	if err := check(context.WithValue(ctx, repositoryTxKey{}, (*repositoryTx)(nil))); err != nil {
		test.Fatal(err)
	}
	if memoryReadComparisonCountPG(test, storage, ctx, job.Target.RepoID) != 0 {
		test.Fatal("borrowed write left receipt")
	}
}

func TestPGMemoryReadComparisonSQLBindingsAndBounds(test *testing.T) {
	storage, ctx, job, _, output := memoryReadWorkOutputWithoutComparisonPG(test)
	comparison := memoryReadComparisonPG(test, ctx, job, output)
	for _, scenario := range []string{"version", "read", "capture", "target", "source", "repo", "expected", "actual", "oversized"} {
		test.Run(scenario, func(test *testing.T) {
			invalid, repo := comparison, job.Target.RepoID
			switch scenario {
			case "version":
				invalid.Version++
			case "read":
				invalid.ReadID = domain.HashContent([]byte("other read"))
			case "capture":
				invalid.CaptureID = domain.HashContent([]byte("other capture"))
			case "target":
				invalid.TargetID = domain.HashContent([]byte("other target"))
			case "source":
				invalid.Source.Sequence++
			case "repo":
				repo = domain.HashContent([]byte("other repo"))
			case "expected":
				invalid.Expected = ""
			case "actual":
				invalid.Actual = "invalid"
			}
			raw := memoryReadComparisonJSONPG(test, invalid)
			if scenario == "oversized" {
				raw = append(raw, bytes.Repeat([]byte(" "), domain.MaxMemoryReadComparisonBytes)...)
			}
			_, err := storage.pool.Exec(ctx, `INSERT INTO memory_read_comparisons(repo_id,read_id,body) VALUES($1,$2,$3)`, string(repo), string(output.ID), raw)
			var databaseError *pgconn.PgError
			if !errors.As(err, &databaseError) || databaseError.Code != "23514" {
				test.Fatal("SQL binding accepted invalid receipt", err)
			}
		})
	}
}

func TestPGMemoryReadComparisonFinalLeaseAndCaptureFences(test *testing.T) {
	for _, scenario := range []string{"cancel", "lease", "capture"} {
		test.Run(scenario, func(test *testing.T) {
			storage, parent, job, _, output := memoryReadWorkOutputWithoutComparisonPG(test)
			comparison := memoryReadComparisonPG(test, parent, job, output)
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "INSERT INTO memory_read_comparisons")
			defer release()
			if scenario == "lease" {
				catalogExecPG(test, parent, storage.pool, `UPDATE memory_read_work SET lease_until=clock_timestamp()+interval '150 milliseconds'
 WHERE repo_id=$1 AND target_id=$2`, string(job.Target.RepoID), string(job.ID))
			}
			if scenario == "capture" {
				memoryBundlePGFault(test, storage, parent, `UPDATE memory_capture_headers SET created_at=clock_timestamp()-interval '1 hour',
 expires_at=clock_timestamp()+interval '150 milliseconds' WHERE repo_id=$1 AND capture_id=$2`, string(job.Target.RepoID), string(comparison.CaptureID))
			}
			done := make(chan error, 1)
			go func() { done <- peer.RecordMemoryReadComparison(ctx, job, comparison) }()
			memoryQualificationWaitPG(test, parent, reached, done)
			wanted := context.Canceled
			switch scenario {
			case "cancel":
				cancel()
			case "lease":
				wanted = domain.ErrConflict
				var remaining float64
				if err := storage.pool.QueryRow(parent, `SELECT GREATEST(0,extract(epoch FROM lease_until-clock_timestamp()))
 FROM memory_read_work WHERE repo_id=$1 AND target_id=$2`, string(job.Target.RepoID), string(job.ID)).Scan(&remaining); err != nil {
					test.Fatal(err)
				}
				time.Sleep(time.Duration(remaining*float64(time.Second)) + 5*time.Millisecond)
			case "capture":
				wanted = domain.ErrMemoryCaptureExpired
				memoryReadPGWaitExpiry(test, storage, parent, job.Target.RepoID, comparison.CaptureID)
			}
			release()
			if err := <-done; !errors.Is(err, wanted) {
				test.Fatal("final fence did not abort comparison", err)
			}
			if memoryReadComparisonCountPG(test, storage, parent, job.Target.RepoID) != 0 {
				test.Fatal("aborted comparison left receipt")
			}
		})
	}
}

func TestPGMemoryReadComparisonCorruptionAndDuplicateGate(test *testing.T) {
	for _, scenario := range []string{"missing", "mismatch", "malformed", "noncanonical", "unknown-field", "version", "read", "capture", "target", "source", "actual", "expected"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, output := memoryReadWorkOutputPG(test)
			identifier, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification)
			if err != nil {
				test.Fatal(err)
			}
			completed := memoryReadWorkStatusPG(test, storage, ctx, job)
			comparison := memoryReadComparisonPG(test, ctx, job, output)
			expectedFinish, expectedRead := domain.ErrIntegrity, domain.ErrIntegrity
			switch scenario {
			case "missing":
				expectedFinish, expectedRead = domain.ErrMemoryReadNotReady, domain.ErrMemoryReadNotReady
			case "mismatch":
				comparison.Expected = domain.HashContent([]byte("mismatching expected"))
				expectedFinish, expectedRead = domain.ErrMemoryReadComparisonMismatch, domain.ErrMemoryReadNeedsAttention
			case "version":
				comparison.Version++
			case "read":
				comparison.ReadID = domain.HashContent([]byte("different read"))
			case "capture":
				comparison.CaptureID = domain.HashContent([]byte("different capture"))
			case "target":
				comparison.TargetID = domain.HashContent([]byte("different target"))
			case "source":
				comparison.Source.Sequence++
			case "actual":
				comparison.Actual = domain.HashContent([]byte("unbound actual"))
				comparison.Expected = comparison.Actual
			case "expected":
				comparison.Expected = ""
			}
			raw := memoryReadComparisonJSONPG(test, comparison)
			switch scenario {
			case "malformed":
				raw = []byte("{")
			case "noncanonical":
				raw = append(raw, ' ')
			case "unknown-field":
				raw = append(raw[:len(raw)-1], []byte(",\"unknown\":true}")...)
			}
			if scenario == "missing" {
				catalogExecPG(test, ctx, storage.pool, `DELETE FROM memory_read_comparisons WHERE repo_id=$1 AND read_id=$2`, string(job.Target.RepoID), string(output.ID))
			} else {
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_read_comparisons SET body=$3 WHERE repo_id=$1 AND read_id=$2`, string(job.Target.RepoID), string(output.ID), raw)
			}
			for _, duplicate := range []domain.MemoryReadWork{job, completed} {
				if actual, err := storage.FinishMinimumMemoryReadWork(ctx, duplicate, qualification); !errors.Is(err, expectedFinish) || actual != "" {
					test.Fatal("duplicate bypassed comparison gate", actual, err)
				}
			}
			if after := memoryReadWorkStatusPG(test, storage, ctx, job); after != completed {
				test.Fatal("failed duplicate altered existing pointer")
			}
			requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
			for _, generation := range []domain.ContentHash{"", identifier} {
				called := false
				err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, generation, memoryQualificationAllowPG,
					func(context.Context, domain.MemoryReadStatus, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
						called = true
						return nil
					})
				if !errors.Is(err, expectedRead) || called {
					test.Fatal("corrupt receipt exposed result", err, called)
				}
			}
		})
	}
}

func TestPGMemoryReadComparisonMismatchRejectionReason(test *testing.T) {
	storage, ctx, job, qualification, output := memoryReadWorkOutputWithoutComparisonPG(test)
	comparison := memoryReadComparisonPG(test, ctx, job, output)
	comparison.Expected = domain.HashContent([]byte("mismatching reference"))
	if err := storage.RecordMemoryReadComparison(ctx, job, comparison); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification); !errors.Is(err, domain.ErrMemoryReadComparisonMismatch) {
		test.Fatal(err)
	}
	if err := storage.FailMemoryReadWork(ctx, job, "comparison_mismatch", false); err != nil {
		test.Fatal(err)
	}
	status := memoryReadWorkStatusPG(test, storage, ctx, job)
	if status.State != "rejected" || status.Reason != "comparison_mismatch" || status.QualificationID != "" || memoryReadComparisonCountPG(test, storage, ctx, job.Target.RepoID) != 1 {
		test.Fatal("mismatch rejection lost diagnostics or published a result", status)
	}
	if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, memoryReadMinimumObservePG(test, storage, ctx, job.Target), "", memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); !errors.Is(err, domain.ErrMemoryReadNeedsAttention) {
		test.Fatal("terminal mismatch was not attention", err)
	}
}

func TestPGMemoryReadComparisonSameEpochAdvance(test *testing.T) {
	storage, ctx, job, qualification, output := memoryReadWorkOutputWithoutComparisonPG(test)
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='after reference capture' WHERE repo_id=$1`, string(job.Target.RepoID))
	memorySourceBlobPG(test, ctx, storage, job.Target.RepoID, "memory", "later unrelated memory")
	before := memorySourceGenerationPG(test, ctx, storage, job.Target.RepoID)
	memoryReadComparisonRecordPG(test, storage, ctx, job, output)
	if _, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification); err != nil {
		test.Fatal("later writes prevented minimum progress", err)
	}
	if after := memorySourceGenerationPG(test, ctx, storage, job.Target.RepoID); after != before {
		test.Fatal("comparison advanced source")
	}
}

func TestPGMemoryReadComparisonScopeSerializesWithRecord(test *testing.T) {
	storage, ctx, job, _, output := memoryReadWorkOutputWithoutComparisonPG(test)
	comparison := memoryReadComparisonPG(test, ctx, job, output)
	peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "INSERT INTO memory_read_comparisons")
	defer release()
	done := make(chan error, 1)
	go func() { done <- peer.RecordMemoryReadComparison(ctx, job, comparison) }()
	memoryQualificationWaitPG(test, ctx, reached, done)
	disabled := make(chan error, 1)
	go func() { disabled <- storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, false) }()
	select {
	case err := <-disabled:
		test.Fatal("disable crossed owned scope lock", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		test.Fatal(err)
	}
	if err := <-disabled; err != nil {
		test.Fatal(err)
	}
	if err := storage.RecordMemoryReadComparison(ctx, job, comparison); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("duplicate bypassed disabled scope", err)
	}
}

func TestPGMemoryReadComparisonRejectedNewerWorkKeepsPriorMinimum(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	identifier, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification)
	if err != nil {
		test.Fatal(err)
	}
	prior := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='new source needs comparison' WHERE repo_id=$1`, string(job.Target.RepoID))
	newer := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
	memoryReadWorkRequestPG(test, storage, ctx, job.Target)
	next := memoryReadWorkClaimPG(test, storage, ctx, job.Target.RepoID)
	if err := storage.FailMemoryReadWork(ctx, next, "comparison_mismatch", false); err != nil {
		test.Fatal(err)
	}
	for _, generation := range []domain.ContentHash{"", identifier} {
		if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, prior, generation, memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); err != nil {
			test.Fatal("later rejection hid accepted prior minimum", err)
		}
		wanted := domain.ErrMemoryReadNeedsAttention
		if generation != "" {
			wanted = domain.ErrMemoryReadNotReady
		}
		called := false
		err := storage.WithMinimumMemoryReadRange(ctx, job.Target, newer, generation, memoryQualificationAllowPG,
			func(context.Context, domain.MemoryReadStatus, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
				called = true
				return nil
			})
		if !errors.Is(err, wanted) || called {
			test.Fatal("rejected newer work became pending or served old source", generation, err, called)
		}
	}
	if status := memoryReadWorkStatusPG(test, storage, ctx, next); status.State != "rejected" || status.Reason != "comparison_mismatch" || status.QualificationID != identifier {
		test.Fatal("minimum reads changed prior pointer or terminal state", status)
	}
}
