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

func TestPGMemoryCaptureBundleCorruption(test *testing.T) {
	for _, scenario := range []string{"missing-part", "missing-block", "payload", "block-digest", "ordinal", "kind", "key", "sequence", "header", "foreign-header", "foreign-block", "count", "total"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			bundle := memoryBundlePGFixture(test, storage, ctx, repo)
			memoryBundlePGWrite(test, storage, ctx, bundle, time.Now().Add(time.Hour))
			part := bundle.Parts[len(bundle.Parts)-1]
			digest := string(domain.HashContent(part.Value))
			switch scenario {
			case "missing-part":
				memoryBundlePGFault(test, storage, ctx, `DELETE FROM memory_capture_parts WHERE repo_id=$1 AND capture_id=$2 AND ordinal=0`, string(repo), string(bundle.ID))
			case "missing-block":
				memoryBundlePGFault(test, storage, ctx, `DELETE FROM memory_capture_blocks WHERE repo_id=$1 AND block_hash=$2`, string(repo), digest)
			case "payload":
				corrupt := bytes.Repeat([]byte("!"), len(part.Value))
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_blocks SET body=$3 WHERE repo_id=$1 AND block_hash=$2`, string(repo), digest, corrupt)
			case "block-digest":
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_parts SET block_hash=$3 WHERE repo_id=$1 AND capture_id=$2 AND ordinal=0`, string(repo), string(bundle.ID), digest)
			case "ordinal":
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_parts SET ordinal=100 WHERE repo_id=$1 AND capture_id=$2 AND ordinal=0`, string(repo), string(bundle.ID))
			case "kind":
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_parts SET kind='graph:snapshot' WHERE repo_id=$1 AND capture_id=$2 AND kind='graph:repository'`, string(repo), string(bundle.ID))
			case "key":
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_parts SET entity_key=$3 WHERE repo_id=$1 AND capture_id=$2 AND ordinal=0`, string(repo), string(bundle.ID), string(domain.HashContent([]byte("other memory"))))
			case "sequence":
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_parts SET source_seq=source_seq+1 WHERE repo_id=$1 AND capture_id=$2 AND kind='graph:repository'`, string(repo), string(bundle.ID))
			case "header", "foreign-header":
				header := bundle.Header
				if scenario == "foreign-header" {
					header.Generation.Graph.RepoID = memorySourceRepoPG(test, ctx, storage, "other header")
				} else {
					header.Generation.Sequence++
				}
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_headers SET header=$3 WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(bundle.ID), memoryCaptureTestJSON(test, header))
			case "foreign-block":
				foreign := memorySourceRepoPG(test, ctx, storage, "other block")
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_blocks SET repo_id=$3 WHERE repo_id=$1 AND block_hash=$2`, string(repo), digest, string(foreign))
			case "count":
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_headers SET part_count=part_count+1 WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(bundle.ID))
			case "total":
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_headers SET body_bytes=body_bytes+1 WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(bundle.ID))
			}
			if actual, err := storage.ReadMemoryCapture(ctx, repo, bundle.ID); !errors.Is(err, domain.ErrIntegrity) || actual.ID != "" || len(actual.Parts) != 0 {
				test.Fatalf("%s returned corrupt capture: %s %v", scenario, actual.ID, err)
			}
			if err := storage.WriteMemoryCapture(ctx, bundle, time.Now().Add(time.Hour)); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatalf("retry silently repaired %s: %v", scenario, err)
			}
		})
	}
}

func TestPGMemoryCaptureBundleConflictingBlockRollback(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	bundle := memoryBundlePGFixture(test, storage, ctx, repo)
	part := bundle.Parts[len(bundle.Parts)-1]
	digest := string(domain.HashContent(part.Value))
	corrupt := []byte("do not silently replace me")
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO memory_capture_blocks(repo_id,block_hash,body) VALUES($1,$2,$3)`, string(repo), digest, corrupt)
	if err := storage.WriteMemoryCapture(ctx, bundle, time.Now().Add(time.Hour)); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("conflicting bytes accepted", err)
	}
	if counts := memoryBundlePGCounts(test, storage, ctx, repo); counts != [3]int{0, 0, 1} {
		test.Fatal("partial write survived rollback", counts)
	}
	var raw []byte
	if err := storage.pool.QueryRow(ctx, `SELECT body FROM memory_capture_blocks WHERE repo_id=$1 AND block_hash=$2`, string(repo), digest).Scan(&raw); err != nil || !bytes.Equal(raw, corrupt) {
		test.Fatal("conflicting block rewritten", err)
	}
}

func TestPGMemoryCaptureBundlePruneRollback(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	bundle := memoryBundlePGFixture(test, storage, ctx, repo)
	memoryBundlePGWrite(test, storage, ctx, bundle, time.Now().Add(time.Hour))
	memoryBundlePGExpire(test, storage, ctx, repo, bundle.ID)
	before := memoryBundlePGCounts(test, storage, ctx, repo)
	catalogExecPG(test, ctx, storage.pool, fmt.Sprintf(`CREATE FUNCTION memory_bundle_prune_fault() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN IF OLD.repo_id='%s' THEN RAISE EXCEPTION 'injected prune failure'; END IF; RETURN OLD; END $$`, repo))
	test.Cleanup(func() {
		_, _ = storage.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS memory_bundle_prune_fault ON memory_capture_headers; DROP FUNCTION IF EXISTS memory_bundle_prune_fault()`)
	})
	catalogExecPG(test, ctx, storage.pool, `CREATE TRIGGER memory_bundle_prune_fault BEFORE DELETE ON memory_capture_headers FOR EACH ROW EXECUTE FUNCTION memory_bundle_prune_fault()`)
	if count, err := storage.PruneMemoryCaptures(ctx, repo, 1); err == nil || count != 0 {
		test.Fatal("failed prune reported success", count, err)
	}
	if after := memoryBundlePGCounts(test, storage, ctx, repo); after != before {
		test.Fatal("failed prune removed blocks/relations", before, after)
	}
}

type memoryBundleSnapshotProbe struct {
	pgx.Tx
	afterPreflight func()
}

func (probe *memoryBundleSnapshotProbe) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	row := probe.Tx.QueryRow(ctx, query, arguments...)
	if query == memoryCapturePreflightSQL {
		probe.afterPreflight()
	}
	return row
}

func TestPGMemoryCaptureBundlePreflightSnapshotPinned(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	bundle := memoryBundlePGFixture(test, storage, ctx, repo)
	memoryBundlePGWrite(test, storage, ctx, bundle, time.Now().Add(time.Hour))
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	called := false
	probe := &memoryBundleSnapshotProbe{Tx: transaction, afterPreflight: func() {
		called = true
		memoryBundlePGFault(test, storage, ctx, `DELETE FROM memory_capture_parts WHERE repo_id=$1 AND capture_id=$2 AND ordinal=0`, string(repo), string(bundle.ID))
	}}
	actual, err := readMemoryCaptureBundle(ctx, probe, repo, bundle.ID)
	if err != nil || actual.ID != bundle.ID || !called {
		test.Fatal("read reopened snapshot after preflight", err)
	}
	if _, err := storage.ReadMemoryCapture(ctx, repo, bundle.ID); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("fresh snapshot did not observe missing part", err)
	}
}

func memoryBundlePGGate(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, statement string) {
	test.Helper()
	catalogExecPG(test, ctx, storage.pool, fmt.Sprintf(`CREATE FUNCTION memory_bundle_test_gate() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN IF NEW.repo_id='%s' THEN %s; END IF; RETURN NEW; END $$`, repo, statement))
	test.Cleanup(func() {
		_, _ = storage.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS memory_bundle_test_gate ON memory_capture_headers; DROP FUNCTION IF EXISTS memory_bundle_test_gate()`)
	})
	catalogExecPG(test, ctx, storage.pool, `CREATE TRIGGER memory_bundle_test_gate BEFORE INSERT ON memory_capture_headers FOR EACH ROW EXECUTE FUNCTION memory_bundle_test_gate()`)
}

func TestPGMemoryCaptureBundleAtomicVisibilityCancellation(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	fixture := memoryBundlePGFixture(test, storage, ctx, repo)
	parts := append([]domain.MemoryCapturePart(nil), fixture.Parts...)
	for index := range 140 {
		parts = append(parts, domain.MemoryCapturePart{Kind: "graph:ref", Key: fmt.Sprintf("ref-%04d", index), Value: []byte(fmt.Sprintf(`{"name":"%04d"}`, index))})
	}
	bundle := memoryBundlePGSeal(test, ctx, fixture.Header, parts)
	lockName := "memory-bundle-test:" + string(repo)
	blocker := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, blocker, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockName)
	memoryBundlePGGate(test, storage, ctx, repo, fmt.Sprintf("PERFORM pg_advisory_xact_lock(hashtextextended('%s',0))", lockName))
	writer, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- storage.WriteMemoryCapture(writer, bundle, time.Now().Add(time.Hour)) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := storage.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted
			AND objid::bigint=(hashtextextended($1,0)&4294967295))`, lockName).Scan(&waiting); err != nil {
			test.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			test.Fatalf("write finished before complete marker: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			test.Fatal("writer never reached publication gate")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if counts := memoryBundlePGCounts(test, storage, ctx, repo); counts != [3]int{} {
		test.Fatal("uncommitted capture visible", counts)
	}
	if _, err := storage.ReadMemoryCapture(ctx, repo, bundle.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("incomplete capture readable", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		test.Fatal("canceled transaction succeeded", err)
	}
	if counts := memoryBundlePGCounts(test, storage, ctx, repo); counts != [3]int{} {
		test.Fatal("canceled batches survived", counts)
	}
	if err := blocker.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	memoryBundlePGWrite(test, storage, ctx, bundle, time.Now().Add(time.Hour))
	memoryBundlePGRead(test, storage, ctx, bundle)
}

func TestPGMemoryCaptureBundleDatabaseConstraints(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	bundle := memoryBundlePGFixture(test, storage, ctx, repo)
	memoryBundlePGWrite(test, storage, ctx, bundle, time.Now().Add(time.Hour))
	for _, query := range []string{
		`UPDATE memory_capture_headers SET expires_at=expires_at+interval '1 minute' WHERE repo_id=$1`,
		`UPDATE memory_capture_blocks SET body=body WHERE repo_id=$1`,
		`UPDATE memory_capture_parts SET source_seq=source_seq WHERE repo_id=$1`,
		`INSERT INTO memory_capture_blocks(repo_id,block_hash,body) VALUES($1,'bad',decode('00','hex'))`,
		`INSERT INTO memory_capture_blocks(repo_id,block_hash,body) VALUES($1,'sha256:'||repeat('f',64),decode(repeat('00',1048577),'hex'))`,
		`INSERT INTO memory_capture_headers(repo_id,capture_id,header,part_count,body_bytes,key_bytes,complete,expires_at)
		 VALUES($1,'sha256:'||repeat('f',64),decode('00','hex'),1,0,0,true,'infinity')`,
		`INSERT INTO memory_capture_headers(repo_id,capture_id,header,part_count,body_bytes,key_bytes,complete,expires_at)
		 VALUES($1,'sha256:'||repeat('f',64),decode('00','hex'),1,0,0,true,clock_timestamp()+interval '25 hours')`,
		`INSERT INTO memory_capture_headers(repo_id,capture_id,header,part_count,body_bytes,key_bytes,complete,expires_at)
		 VALUES($1,'sha256:'||repeat('f',64),decode('00','hex'),1,0,0,false,clock_timestamp()+interval '1 hour')`,
	} {
		if _, err := storage.pool.Exec(ctx, query, string(repo)); err == nil {
			test.Fatalf("constraint missing: %s", query)
		}
	}
	foreign := memorySourceRepoPG(test, ctx, storage, "fk")
	transaction := catalogTxPG(test, ctx, storage)
	if _, err := transaction.Exec(ctx, `INSERT INTO memory_capture_parts(repo_id,capture_id,ordinal,kind,entity_key,source_seq,block_hash)
		VALUES($1,$2,0,'absent','key',0,$3)`, string(foreign), string(bundle.ID), string(domain.HashContent(bundle.Parts[0].Value))); err == nil {
		test.Fatal("cross-repository block foreign key accepted")
	}
	transaction = catalogTxPG(test, ctx, storage)
	if _, err := transaction.Exec(ctx, `INSERT INTO memory_capture_parts(repo_id,capture_id,ordinal,kind,entity_key,source_seq,block_hash)
		VALUES($1,$2,0,'absent','key',0,$3)`, string(repo), string(domain.HashContent([]byte("uncompleted"))), string(domain.HashContent(bundle.Parts[0].Value))); err != nil {
		test.Fatal(err)
	}
	if err := transaction.Commit(ctx); err == nil {
		test.Fatal("parts committed without complete header")
	}
}

func TestPGMemoryCaptureBundleBoundedTransferSQL(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	bundle := memoryBundlePGFixture(test, storage, ctx, repo)
	memoryBundlePGWrite(test, storage, ctx, bundle, time.Now().Add(time.Hour))
	for _, limits := range []struct {
		name                        string
		body, key, kind, cumulative int
	}{
		{"body", 1, memoryCaptureKeyBytes, memoryCaptureKindBytes, domain.MaxMemoryCaptureBytes},
		{"key", domain.MaxMemoryCapturePartBytes, 1, memoryCaptureKindBytes, domain.MaxMemoryCaptureBytes},
		{"kind", domain.MaxMemoryCapturePartBytes, memoryCaptureKeyBytes, 1, domain.MaxMemoryCaptureBytes},
		{"cumulative", domain.MaxMemoryCapturePartBytes, memoryCaptureKeyBytes, memoryCaptureKindBytes, 256},
	} {
		test.Run(limits.name, func(test *testing.T) {
			rows, err := storage.pool.Query(ctx, memoryCapturePartsSQL, string(repo), string(bundle.ID), domain.MaxMemoryCaptureParts+1, limits.body, limits.key, limits.kind, limits.cumulative)
			if err != nil {
				test.Fatal(err)
			}
			defer rows.Close()
			var total int64
			suppressed := 0
			for rows.Next() {
				var ordinal, sequence, bodySize, kindSize, keySize int64
				var digest, kind, key *string
				var body []byte
				if err := rows.Scan(&ordinal, &sequence, &bodySize, &kindSize, &keySize, &digest, &kind, &key, &body); err != nil {
					test.Fatal(err)
				}
				total += bodySize + kindSize + keySize + 128
				if bodySize > int64(limits.body) || keySize > int64(limits.key) || kindSize > int64(limits.kind) || total > int64(limits.cumulative) {
					if body != nil {
						test.Fatal("oversized/cumulative payload transferred")
					}
					suppressed++
				}
				if keySize > int64(limits.key) && key != nil || kindSize > int64(limits.kind) && kind != nil {
					test.Fatal("oversized metadata transferred")
				}
			}
			if err := rows.Err(); err != nil || suppressed == 0 {
				test.Fatal("CASE suppression untested", err)
			}
		})
	}
}

func TestPGMemoryCaptureBundleReadPreflightLimits(test *testing.T) {
	for _, scenario := range []string{"body", "key", "kind", "count", "cumulative", "header"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			bundle := memoryBundlePGFixture(test, storage, ctx, repo)
			memoryBundlePGWrite(test, storage, ctx, bundle, time.Now().Add(time.Hour))
			transaction := catalogTxPG(test, ctx, storage)
			for _, table := range []string{"memory_capture_headers", "memory_capture_parts", "memory_capture_blocks"} {
				constraints, err := transaction.Query(ctx, `SELECT conname FROM pg_constraint WHERE conrelid=$1::regclass AND contype='c'`, table)
				if err != nil {
					test.Fatal(err)
				}
				names, err := pgx.CollectRows(constraints, pgx.RowTo[string])
				if err != nil {
					test.Fatal(err)
				}
				for _, name := range names {
					catalogExecPG(test, ctx, transaction, `ALTER TABLE `+pgx.Identifier{table}.Sanitize()+` DROP CONSTRAINT `+pgx.Identifier{name}.Sanitize())
				}
			}
			catalogExecPG(test, ctx, transaction, `SET LOCAL session_replication_role=replica`)
			switch scenario {
			case "body":
				catalogExecPG(test, ctx, transaction, `UPDATE memory_capture_blocks SET body=$2 WHERE repo_id=$1`, string(repo), []byte(strings.Repeat("x", domain.MaxMemoryCapturePartBytes+1)))
			case "key":
				catalogExecPG(test, ctx, transaction, `UPDATE memory_capture_parts SET entity_key=entity_key||$2 WHERE repo_id=$1`, string(repo), strings.Repeat("x", memoryCaptureKeyBytes+1))
			case "kind":
				catalogExecPG(test, ctx, transaction, `UPDATE memory_capture_parts SET kind=kind||$2 WHERE repo_id=$1`, string(repo), strings.Repeat("x", memoryCaptureKindBytes+1))
			case "header":
				catalogExecPG(test, ctx, transaction, `UPDATE memory_capture_headers SET header=$2 WHERE repo_id=$1`, string(repo), []byte(strings.Repeat("x", memoryCaptureHeaderBytes+1)))
			case "count", "cumulative":
				count := domain.MaxMemoryCaptureParts + 1
				if scenario == "cumulative" {
					count = 120
				}
				body := []byte(strings.Repeat("x", domain.MaxMemoryCapturePartBytes))
				digest := string(domain.HashContent(body))
				catalogExecPG(test, ctx, transaction, `INSERT INTO memory_capture_blocks(repo_id,block_hash,body) VALUES($1,$2,$3)`, string(repo), digest, body)
				catalogExecPG(test, ctx, transaction, `INSERT INTO memory_capture_parts(repo_id,capture_id,ordinal,kind,entity_key,source_seq,block_hash)
					SELECT $1,$2,1000+ordinal,'graph:ref',ordinal::text,0,$3 FROM generate_series(1,$4) ordinal`, string(repo), string(bundle.ID), digest, count)
			}
			if actual, err := readMemoryCaptureBundle(ctx, transaction, repo, bundle.ID); !errors.Is(err, domain.ErrMemoryProjectionLimit) || actual.ID != "" {
				test.Fatal("preflight failed to reject", scenario, err)
			}
		})
	}
}
