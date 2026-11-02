//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGMemoryReadCorruptionNeverRepairs(test *testing.T) {
	for _, scenario := range []string{"body", "chunk-hash", "chunk-start", "chunk-count", "missing", "header", "header-count", "total-bytes", "capture-pointer", "capture-source", "capture-selection", "capture-content"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 65)
			memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
			memoryReadPGWrite(test, storage, ctx, bundle)
			query := ""
			arguments := []any{string(repo), string(bundle.ID)}
			switch scenario {
			case "body":
				query = `UPDATE memory_read_chunks SET body=set_byte(body,10,120) WHERE repo_id=$1 AND read_id=$2 AND ordinal=0`
			case "chunk-hash":
				query = `UPDATE memory_read_chunks SET chunk_hash='sha256:'||repeat('a',64) WHERE repo_id=$1 AND read_id=$2 AND ordinal=0`
			case "chunk-start":
				query = `UPDATE memory_read_chunks SET start_item=start_item+1 WHERE repo_id=$1 AND read_id=$2 AND ordinal=0`
			case "chunk-count":
				query = `UPDATE memory_read_chunks SET item_count=item_count-1 WHERE repo_id=$1 AND read_id=$2 AND ordinal=0`
			case "missing":
				query = `DELETE FROM memory_read_chunks WHERE repo_id=$1 AND read_id=$2 AND ordinal=0`
			case "header":
				query = `UPDATE memory_read_headers SET header=set_byte(header,10,120) WHERE repo_id=$1 AND read_id=$2`
			case "header-count":
				query = `UPDATE memory_read_headers SET chunk_count=chunk_count+1 WHERE repo_id=$1 AND read_id=$2`
			case "total-bytes":
				query = `UPDATE memory_read_headers SET body_bytes=body_bytes+1 WHERE repo_id=$1 AND read_id=$2`
			case "capture-pointer":
				query = `UPDATE memory_read_headers SET capture_id='sha256:'||repeat('a',64) WHERE repo_id=$1 AND read_id=$2`
			default:
				header := capture.Header
				switch scenario {
				case "capture-source":
					header.Generation.Sequence++
				case "capture-selection":
					header.Selection.CodeCommit = strings.Repeat("b", 40)
				case "capture-content":
					header.Content = "claims"
				}
				query = `UPDATE memory_capture_headers SET header=$3 WHERE repo_id=$1 AND capture_id=$2`
				arguments = []any{string(repo), string(capture.ID), memoryCaptureTestJSON(test, header)}
			}
			memoryBundlePGFault(test, storage, ctx, query, arguments...)
			before := memoryReadPGCounts(test, storage, ctx, repo)
			if scenario != "body" {
				if _, err := storage.ReadMemoryReadHeader(ctx, repo, bundle.ID); !errors.Is(err, domain.ErrIntegrity) {
					test.Fatal("corrupt header/index accepted", err)
				}
			}
			if raw, err := storage.ReadMemoryReadChunk(ctx, repo, bundle.ID, 0); !errors.Is(err, domain.ErrIntegrity) || raw != nil {
				test.Fatal("corrupt chunk accepted", err)
			}
			if err := storage.WriteMemoryRead(ctx, bundle); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("corrupt retry accepted", err)
			}
			if after := memoryReadPGCounts(test, storage, ctx, repo); after != before {
				test.Fatal("retry changed rows", before, after)
			}
			if raw, err := storage.ReadMemoryReadChunk(ctx, repo, bundle.ID, 0); !errors.Is(err, domain.ErrIntegrity) || raw != nil {
				test.Fatal("retry repaired data", err)
			}
		})
	}
}

func TestPGMemoryReadOnlySelectedPayloadIsVerified(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 65)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	memoryReadPGWrite(test, storage, ctx, bundle)
	memoryBundlePGFault(test, storage, ctx, `UPDATE memory_read_chunks SET body=set_byte(body,10,120) WHERE repo_id=$1 AND read_id=$2 AND ordinal=1`, string(repo), string(bundle.ID))
	memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_blocks SET body=decode('00','hex') WHERE repo_id=$1 AND $2::text<>''`, string(repo), string(capture.ID))
	if _, err := storage.ReadMemoryReadHeader(ctx, repo, bundle.ID); err != nil {
		test.Fatal("header fetched capture/result payload", err)
	}
	if raw, err := storage.ReadMemoryReadChunk(ctx, repo, bundle.ID, 0); err != nil || !bytes.Equal(raw, bundle.Chunks[0].Bytes) {
		test.Fatal("selected read fetched unrelated payload", err)
	}
	if raw, err := storage.ReadMemoryReadChunk(ctx, repo, bundle.ID, 1); !errors.Is(err, domain.ErrIntegrity) || raw != nil {
		test.Fatal("selected payload not verified", err)
	}
}

func TestPGMemoryReadStrictSelectedDecode(test *testing.T) {
	for _, scenario := range []string{"count", "unknown", "duplicate", "trailing"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 1)
			memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
			memoryReadPGWrite(test, storage, ctx, bundle)
			raw := bytes.Clone(bundle.Chunks[0].Bytes)
			switch scenario {
			case "count":
				raw = append(append(append([]byte{'['}, raw[1:len(raw)-1]...), ','), raw[1:]...)
			case "unknown":
				raw = bytes.Replace(raw, []byte(`[{`), []byte(`[{"unknown":true,`), 1)
			case "duplicate":
				raw = bytes.Replace(raw, []byte(`[{`), []byte(`[{"kind":"rationale",`), 1)
			case "trailing":
				raw = append(raw, []byte(` []`)...)
			}
			header := bundle.Header
			header.Chunks[0].Hash, header.Chunks[0].Bytes = domain.HashContent(raw), len(raw)
			headerBytes, err := domain.MemoryReadHeaderBytes(ctx, header)
			if err != nil {
				test.Fatal(err)
			}
			identifier := domain.HashContent(headerBytes)
			transaction := catalogTxPG(test, ctx, storage)
			catalogExecPG(test, ctx, transaction, `SET LOCAL session_replication_role=replica`)
			catalogExecPG(test, ctx, transaction, `UPDATE memory_read_headers SET read_id=$3,header=$4,body_bytes=$5 WHERE repo_id=$1 AND read_id=$2`, string(repo), string(bundle.ID), string(identifier), headerBytes, len(raw))
			catalogExecPG(test, ctx, transaction, `UPDATE memory_read_chunks SET read_id=$3,chunk_hash=$4,body=$5 WHERE repo_id=$1 AND read_id=$2`, string(repo), string(bundle.ID), string(identifier), string(domain.HashContent(raw)), raw)
			if err := transaction.Commit(ctx); err != nil {
				test.Fatal(err)
			}
			if _, err := storage.ReadMemoryReadHeader(ctx, repo, identifier); err != nil {
				test.Fatal("metadata fixture invalid", err)
			}
			if actual, err := storage.ReadMemoryReadChunk(ctx, repo, identifier, 0); !errors.Is(err, domain.ErrIntegrity) || actual != nil {
				test.Fatal("strict chunk decode bypassed", err)
			}
		})
	}
}

func memoryReadPGInsertHeader(test *testing.T, ctx context.Context, transaction pgx.Tx, bundle domain.MemoryReadBundle, count int, bodyBytes int) {
	test.Helper()
	raw, err := domain.MemoryReadHeaderBytes(ctx, bundle.Header)
	if err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, transaction, `INSERT INTO memory_read_headers(repo_id,read_id,capture_id,header,chunk_count,item_count,body_bytes)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, string(bundle.Header.Source.Graph.RepoID), string(bundle.ID), string(bundle.Header.CaptureID), raw, count, bundle.Header.View.Total, bodyBytes)
}

func TestPGMemoryReadDatabaseConstraints(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 1)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	memoryReadPGWrite(test, storage, ctx, bundle)
	for _, query := range []string{
		`UPDATE memory_read_headers SET body_bytes=body_bytes WHERE repo_id=$1 AND read_id=$2`,
		`UPDATE memory_read_chunks SET body=body WHERE repo_id=$1 AND read_id=$2`,
		`DELETE FROM memory_read_chunks WHERE repo_id=$1 AND read_id=$2`,
		`INSERT INTO memory_read_chunks(repo_id,read_id,ordinal,chunk_hash,start_item,item_count,body)
		 VALUES($1,$2,1,'sha256:'||repeat('a',64),1,1,'[]')`,
		`INSERT INTO memory_read_chunks(repo_id,read_id,ordinal,chunk_hash,start_item,item_count,body)
		 VALUES($1,'sha256:'||repeat('b',64),1,$2,1,1,decode(repeat('00',262147),'hex'))`,
		`INSERT INTO memory_read_chunks(repo_id,read_id,ordinal,chunk_hash,start_item,item_count,body)
		 VALUES($1,'sha256:'||repeat('b',64),1024,$2,1,1,'[]')`,
		`INSERT INTO memory_read_chunks(repo_id,read_id,ordinal,chunk_hash,start_item,item_count,body)
		 VALUES($1,'sha256:'||repeat('b',64),0,$2,1,65,'[]')`,
		`INSERT INTO memory_read_headers(repo_id,read_id,capture_id,header,chunk_count,item_count,body_bytes)
		 SELECT repo_id,'sha256:'||repeat('b',64),capture_id,decode(repeat('00',4194305),'hex'),0,0,0 FROM memory_read_headers WHERE repo_id=$1 AND read_id=$2`,
		`INSERT INTO memory_read_headers(repo_id,read_id,capture_id,header,chunk_count,item_count,body_bytes)
		 SELECT repo_id,'sha256:'||repeat('b',64),capture_id,header,1025,1025,0 FROM memory_read_headers WHERE repo_id=$1 AND read_id=$2`,
		`INSERT INTO memory_read_headers(repo_id,read_id,capture_id,header,chunk_count,item_count,body_bytes)
		 SELECT repo_id,'sha256:'||repeat('b',64),capture_id,header,1,1,67108864 FROM memory_read_headers WHERE repo_id=$1 AND read_id=$2`,
	} {
		if _, err := storage.pool.Exec(ctx, query, string(repo), string(bundle.ID)); err == nil {
			test.Fatalf("constraint accepted: %s", query)
		}
	}
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM memory_read_headers WHERE repo_id=$1 AND read_id=$2`, string(repo), string(bundle.ID))
	if counts := memoryReadPGCounts(test, storage, ctx, repo); counts != [2]int{} {
		test.Fatal("header deletion left orphan chunks", counts)
	}
	transaction := catalogTxPG(test, ctx, storage)
	memoryReadPGInsertHeader(test, ctx, transaction, bundle, 1, len(bundle.Chunks[0].Bytes))
	if err := transaction.Commit(ctx); err == nil {
		test.Fatal("incomplete header committed")
	}
	transaction = catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, transaction, `INSERT INTO memory_read_chunks(repo_id,read_id,ordinal,chunk_hash,start_item,item_count,body)
		VALUES($1,$2,0,$3,0,1,$4)`, string(repo), string(bundle.ID), string(bundle.Chunks[0].Hash), bundle.Chunks[0].Bytes)
	if err := transaction.Commit(ctx); err == nil {
		test.Fatal("orphan chunk committed")
	}
	head := bundle.Header
	head.Source.Sequence++
	invalid := memoryReadPGSeal(test, ctx, head, nil)
	transaction = catalogTxPG(test, ctx, storage)
	memoryReadPGInsertHeader(test, ctx, transaction, invalid, 0, 0)
	if err := transaction.Commit(ctx); err == nil {
		test.Fatal("mismatched capture committed via SQL")
	}
}

func memoryReadPGGate(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, statement string) {
	test.Helper()
	catalogExecPG(test, ctx, storage.pool, fmt.Sprintf(`CREATE FUNCTION memory_read_test_gate() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN IF NEW.repo_id='%s' THEN %s; END IF; RETURN NEW; END $$`, repo, statement))
	test.Cleanup(func() {
		_, _ = storage.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS memory_read_test_gate ON memory_read_headers; DROP FUNCTION IF EXISTS memory_read_test_gate()`)
	})
	catalogExecPG(test, ctx, storage.pool, `CREATE TRIGGER memory_read_test_gate BEFORE INSERT ON memory_read_headers FOR EACH ROW EXECUTE FUNCTION memory_read_test_gate()`)
}

func TestPGMemoryReadAtomicRollback(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 65*domain.MaxMemoryReadChunkItems)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	memoryReadPGGate(test, storage, ctx, repo, "RAISE EXCEPTION 'injected header failure'")
	if err := storage.WriteMemoryRead(ctx, bundle); err == nil {
		test.Fatal("injected write succeeded")
	}
	if counts := memoryReadPGCounts(test, storage, ctx, repo); counts != [2]int{} {
		test.Fatal("partial chunks/header survived", counts)
	}
	if _, err := storage.ReadMemoryReadHeader(ctx, repo, bundle.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("failed result visible", err)
	}
	catalogExecPG(test, ctx, storage.pool, `DROP TRIGGER memory_read_test_gate ON memory_read_headers`)
	memoryReadPGWrite(test, storage, ctx, bundle)
	memoryReadPGAssert(test, storage, ctx, bundle)
}
