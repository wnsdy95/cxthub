//go:build postgres

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func memoryGraphBasePGQualified(test *testing.T) (*PostgresStore, context.Context, domain.MemoryCaptureBundle, domain.MemoryCaptureBundle, domain.MemoryReadBundle, domain.MemoryReadQualification) {
	test.Helper()
	storage, ctx, qualification, output := memoryQualificationPG(test)
	anchor, err := storage.ReadMemoryCapture(ctx, qualification.Source.Graph.RepoID, qualification.CaptureID)
	if err != nil {
		test.Fatal(err)
	}
	child := memoryGraphBasePGDelta(test, ctx, anchor, false)
	child.Header.Generation = anchor.Header.Generation
	child = memoryBundlePGSeal(test, ctx, child.Header, child.Parts)
	memoryBundlePGWrite(test, storage, ctx, child, time.Now().Add(30*time.Minute))
	var items []domain.EffectiveMemoryItem
	for ordinal, chunk := range output.Chunks {
		decoded, err := domain.DecodeMemoryReadChunk(ctx, output.Header, ordinal, chunk.Bytes)
		if err != nil {
			test.Fatal(err)
		}
		items = append(items, decoded...)
	}
	output.Header.CaptureID, output.Header.Source = child.ID, child.Header.Generation
	output = memoryReadPGSeal(test, ctx, output.Header, items)
	memoryReadPGWrite(test, storage, ctx, output)
	qualification.CaptureID, qualification.ReadID, qualification.Source = child.ID, output.ID, child.Header.Generation
	return storage, ctx, anchor, child, output, qualification
}

type memoryGraphBaseHeaderProbe struct {
	pgx.Tx
	test    *testing.T
	headers int
}

func (probe *memoryGraphBaseHeaderProbe) check(query string) {
	probe.test.Helper()
	if strings.Contains(query, "memory_capture_parts") || strings.Contains(query, "memory_capture_blocks") || strings.Contains(query, "graph_projection_inputs") {
		probe.test.Fatal("page read hydrated capture or mutable graph inputs", query)
	}
	if strings.Contains(query, "FROM memory_capture_headers WHERE") {
		probe.headers++
	}
}

func (probe *memoryGraphBaseHeaderProbe) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	probe.check(query)
	return probe.Tx.QueryRow(ctx, query, arguments...)
}

func (probe *memoryGraphBaseHeaderProbe) Query(ctx context.Context, query string, arguments ...any) (pgx.Rows, error) {
	probe.check(query)
	return probe.Tx.Query(ctx, query, arguments...)
}

func TestPGMemoryCaptureGraphBaseHeaderOnlyPagesAndQualification(test *testing.T) {
	storage, ctx, anchor, child, output, qualification := memoryGraphBasePGQualified(test)
	repo := child.Header.Generation.Graph.RepoID
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	probe := &memoryGraphBaseHeaderProbe{Tx: transaction, test: test}
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: probe, owner: storage, readOnly: true, repo: repo})
	if _, err := storage.ReadMemoryReadChunk(bound, repo, output.ID, 0); err != nil || probe.headers != 2 {
		test.Fatal("child and anchor headers not independently checked", probe.headers, err)
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	test.Logf("selected page checks %d bounded capture headers and zero capture-part/block queries", probe.headers)
	memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_blocks SET body=convert_to('{}','UTF8')
		WHERE repo_id=$1 AND block_hash IN (SELECT block_hash FROM memory_capture_parts WHERE repo_id=$1 AND capture_id=$2 AND kind='graph:repository')`,
		string(repo), string(anchor.ID))
	memoryBundlePGRead(test, storage, ctx, child)
	if _, err := storage.ReadMemoryCapture(ctx, repo, anchor.ID); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("cold anchor root stopped validating", err)
	}
	memoryReadPGAssert(test, storage, ctx, output)
	identifier, err := storage.QualifyMemoryRead(ctx, qualification)
	if err != nil {
		test.Fatal("qualification unnecessarily hydrated full anchor", err)
	}
	if err := storage.WithQualifiedMemoryReadRange(ctx, repo, identifier, memoryQualificationAllowPG,
		func(bound context.Context, header domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
			if header.CaptureID != child.ID {
				test.Fatal("qualification changed capture identity")
			}
			_, err := chunks.ReadChunk(bound, 0)
			return err
		}); err != nil {
		test.Fatal(err)
	}
}

func TestPGMemoryCaptureGraphBaseHeaderCorruptionAndExpiry(test *testing.T) {
	for _, scenario := range []string{"expiry-mismatch", "anchor-expired", "child-expired", "anchor-epoch", "child-sequence", "missing-anchor"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, anchor, child, output, qualification := memoryGraphBasePGQualified(test)
			repo := child.Header.Generation.Graph.RepoID
			identifier, err := storage.QualifyMemoryRead(ctx, qualification)
			if err != nil {
				test.Fatal(err)
			}
			expected := domain.ErrIntegrity
			switch scenario {
			case "expiry-mismatch":
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_headers SET expires_at=clock_timestamp()+interval '10 minutes'
					WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(anchor.ID))
			case "anchor-expired":
				memoryBundlePGExpire(test, storage, ctx, repo, anchor.ID)
				expected = domain.ErrMemoryCaptureExpired
			case "child-expired":
				memoryBundlePGExpire(test, storage, ctx, repo, child.ID)
				expected = domain.ErrMemoryCaptureExpired
			case "anchor-epoch":
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_headers SET graph_epoch='11111111-1111-4111-8111-111111111111'
					WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(anchor.ID))
			case "child-sequence":
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_headers SET graph_sequence=graph_sequence+1
					WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(child.ID))
			case "missing-anchor":
				memoryBundlePGFault(test, storage, ctx, `DELETE FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(anchor.ID))
				expected = domain.ErrNotFound
			}
			if _, err := storage.ReadMemoryCapture(ctx, repo, child.ID); !errors.Is(err, expected) {
				test.Fatal("cold child", err)
			}
			if _, err := storage.ReadMemoryReadHeader(ctx, repo, output.ID); !errors.Is(err, expected) {
				test.Fatal("page header", err)
			}
			if _, err := storage.ReadMemoryReadChunk(ctx, repo, output.ID, 0); !errors.Is(err, expected) {
				test.Fatal("page chunk", err)
			}
			if _, err := storage.QualifyMemoryRead(ctx, qualification); !errors.Is(err, expected) {
				test.Fatal("qualification accepted corrupt anchor", err)
			}
			called := false
			if err := storage.WithQualifiedMemoryReadRange(ctx, repo, identifier, memoryQualificationAllowPG,
				func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
					called = true
					return nil
				}); !errors.Is(err, expected) || called {
				test.Fatal("qualified read accepted corrupt anchor", err, called)
			}
		})
	}
}

func TestPGMemoryCaptureGraphBaseFinalClockChecks(test *testing.T) {
	storage, ctx, anchor, child, output, _ := memoryGraphBasePGQualified(test)
	repo := child.Header.Generation.Graph.RepoID
	memoryReadPGExpireSoon(test, storage, ctx, repo, child.ID)
	memoryReadPGExpireSoon(test, storage, ctx, repo, anchor.ID)
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	probe := &memoryReadSnapshotProbe{Tx: transaction, afterChunk: func() { memoryReadPGWaitExpiry(test, storage, ctx, repo, anchor.ID) }}
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: probe, owner: storage, readOnly: true, repo: repo})
	if raw, err := storage.ReadMemoryReadChunk(bound, repo, output.ID, 0); !errors.Is(err, domain.ErrMemoryCaptureExpired) || raw != nil {
		test.Fatal("page final check ignored child/anchor wall-clock expiry", err)
	}
}
