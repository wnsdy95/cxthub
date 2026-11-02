//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func memoryGraphBasePGDelta(test *testing.T, ctx context.Context, anchor domain.MemoryCaptureBundle, changed bool) domain.MemoryCaptureBundle {
	test.Helper()
	header := anchor.Header
	header.Version = domain.MemoryCaptureDeltaVersion
	header.Generation.Sequence++
	parts := []domain.MemoryCapturePart{{Kind: "graph_ref", Key: string(anchor.ID), Sequence: anchor.Header.Generation.Graph.Sequence,
		Value: memoryCaptureTestJSON(test, anchor.Header.Generation.Graph)}}
	for _, part := range anchor.Parts {
		if !strings.HasPrefix(part.Kind, "graph:") {
			parts = append(parts, part)
		}
	}
	if changed {
		header.Generation.Graph.Sequence++
		parts = append(parts,
			domain.MemoryCapturePart{Kind: "graph_delta:repository", Key: string(header.Generation.Graph.RepoID), Sequence: header.Generation.Graph.Sequence, Value: []byte(`{"changed":true}`)},
			domain.MemoryCapturePart{Kind: "graph_delta:ref", Key: "deleted-ref", Sequence: header.Generation.Graph.Sequence})
	}
	return memoryBundlePGSeal(test, ctx, header, parts)
}

func memoryGraphBasePGInsert(ctx context.Context, transaction pgx.Tx, bundle domain.MemoryCaptureBundle, expiry time.Time, version int, epoch string, sequence int64, anchor domain.ContentHash) error {
	var bodyBytes, keyBytes int64
	for _, part := range bundle.Parts {
		bodyBytes += int64(len(part.Value))
		keyBytes += int64(len(part.Kind) + len(part.Key))
	}
	for start := 0; start < len(bundle.Parts); {
		end := memoryCaptureBatchEnd(bundle.Parts, start)
		if err := writeMemoryCaptureBatch(ctx, transaction, bundle, start, end); err != nil {
			return err
		}
		start = end
	}
	header, err := json.Marshal(bundle.Header)
	if err != nil {
		return err
	}
	_, err = transaction.Exec(ctx, `INSERT INTO memory_capture_headers
		(repo_id,capture_id,header,part_count,body_bytes,key_bytes,complete,expires_at,capture_version,graph_epoch,graph_sequence,graph_anchor_id)
		VALUES($1,$2,$3,$4,$5,$6,true,$7,$8,$9,$10,NULLIF($11,''))`, string(bundle.Header.Generation.Graph.RepoID), string(bundle.ID),
		header, len(bundle.Parts), bodyBytes, keyBytes, expiry, version, epoch, sequence, string(anchor))
	return err
}

func TestPGMemoryCaptureGraphBaseRestartAndReferenceCost(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	anchor := memoryBundlePGFixture(test, storage, ctx, repo)
	for ordinal := range 300 {
		anchor.Parts = append(anchor.Parts, domain.MemoryCapturePart{Kind: "graph:ref", Key: fmt.Sprintf("ref-%04d", ordinal),
			Sequence: anchor.Header.Generation.Graph.Sequence, Value: []byte(`{"value":"shared"}`)})
	}
	anchor = memoryBundlePGSeal(test, ctx, anchor.Header, anchor.Parts)
	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	memoryBundlePGWrite(test, storage, ctx, anchor, expiry)
	before := memoryBundlePGCounts(test, storage, ctx, repo)
	unchanged := memoryGraphBasePGDelta(test, ctx, anchor, false)
	changed := memoryGraphBasePGDelta(test, ctx, anchor, true)
	for _, child := range []domain.MemoryCaptureBundle{unchanged, changed} {
		memoryBundlePGWrite(test, storage, ctx, child, expiry)
		memoryBundlePGWrite(test, storage, ctx, child, expiry.Add(time.Hour))
		memoryBundlePGRead(test, storage, ctx, child)
	}
	after := memoryBundlePGCounts(test, storage, ctx, repo)
	if after[0] != 3 || after[1]-before[1] != len(unchanged.Parts)+len(changed.Parts) || len(unchanged.Parts) >= 10 || after[2]-before[2] != 2 {
		test.Fatalf("graph copied or retry mutated storage: before=%v after=%v child_parts=%d", before, after, len(unchanged.Parts))
	}
	var references, copied int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind='graph_ref'),count(*) FILTER (WHERE kind LIKE 'graph:%')
		FROM memory_capture_parts WHERE repo_id=$1 AND capture_id<>$2`, string(repo), string(anchor.ID)).Scan(&references, &copied); err != nil || references != 2 || copied != 0 {
		test.Fatal("reference cost", references, copied, err)
	}
	storage.Close()
	peer, err := NewPostgresStore(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(peer.Close)
	for _, bundle := range []domain.MemoryCaptureBundle{anchor, unchanged, changed} {
		memoryBundlePGRead(test, peer, ctx, bundle)
	}
	if identifier, err := peer.FindMemoryCaptureAnchor(ctx, changed.Header.Generation.Graph, expiry); err != nil || identifier != anchor.ID {
		test.Fatal("restart discovery", identifier, err)
	}
	if applied, err := peer.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil || applied != 0 {
		test.Fatal("migration restart", applied, err)
	}
}

func TestPGMemoryCaptureGraphBaseIndexedDiscoveryAndLegacy(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	anchor := memoryBundlePGFixture(test, storage, ctx, repo)
	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	memoryBundlePGWrite(test, storage, ctx, anchor, expiry)
	memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_headers SET capture_version=NULL,graph_epoch=NULL,graph_sequence=NULL WHERE repo_id=$1`, string(repo))
	memoryBundlePGRead(test, storage, ctx, anchor)
	if _, err := storage.FindMemoryCaptureAnchor(ctx, anchor.Header.Generation.Graph, expiry); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("legacy capture unexpectedly indexed", err)
	}
	header := anchor.Header
	header.Generation.Sequence++
	indexed := memoryBundlePGSeal(test, ctx, header, anchor.Parts)
	memoryBundlePGWrite(test, storage, ctx, indexed, expiry)
	header.Generation.Graph.Sequence += 5
	latest := memoryBundlePGSeal(test, ctx, header, anchor.Parts)
	memoryBundlePGWrite(test, storage, ctx, latest, expiry.Add(-10*time.Minute))
	for _, check := range []struct {
		checkpoint domain.RepositoryViewSourceCheckpoint
		minimum    time.Time
		expected   domain.ContentHash
	}{
		{indexed.Header.Generation.Graph, expiry, indexed.ID},
		{latest.Header.Generation.Graph, expiry.Add(-20 * time.Minute), latest.ID},
		{latest.Header.Generation.Graph, expiry, indexed.ID},
	} {
		identifier, err := storage.FindMemoryCaptureAnchor(ctx, check.checkpoint, check.minimum)
		if err != nil || identifier != check.expected {
			test.Fatal("indexed candidate", identifier, check.expected, err)
		}
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		identifier, err := storage.FindMemoryCaptureAnchor(bound, latest.Header.Generation.Graph, expiry)
		if err != nil || identifier != indexed.ID {
			test.Fatal("borrowed RR discovery", identifier, err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []string{"expiry", "epoch", "repo"} {
		checkpoint, minimum := latest.Header.Generation.Graph, expiry
		switch scenario {
		case "expiry":
			minimum = expiry.Add(time.Minute)
		case "epoch":
			checkpoint.Epoch = "11111111-1111-4111-8111-111111111111"
		case "repo":
			checkpoint.RepoID = domain.HashContent([]byte("other repository"))
		}
		if _, err := storage.FindMemoryCaptureAnchor(ctx, checkpoint, minimum); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal(scenario, err)
		}
	}
	transaction := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, transaction, `SET LOCAL enable_seqscan=off`)
	rows, err := transaction.Query(ctx, `EXPLAIN (COSTS OFF) `+memoryCaptureFindAnchorSQL, string(repo), header.Generation.Graph.Epoch, header.Generation.Graph.Sequence, expiry)
	if err != nil {
		test.Fatal(err)
	}
	plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || !strings.Contains(strings.Join(plan, "\n"), "memory_capture_graph_anchors") || strings.Contains(strings.Join(plan, "\n"), "Sort") {
		test.Fatal("anchor discovery lost ordered index", plan, err)
	}
}

func TestPGMemoryCaptureGraphBaseWriteRejections(test *testing.T) {
	for _, scenario := range []string{"expiry", "checkpoint", "epoch", "chain", "cross-repo", "missing"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			anchor := memoryBundlePGFixture(test, storage, ctx, repo)
			expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
			memoryBundlePGWrite(test, storage, ctx, anchor, expiry)
			child := memoryGraphBasePGDelta(test, ctx, anchor, true)
			base := anchor.Header.Generation.Graph
			identifier, expected := anchor.ID, domain.ErrIntegrity
			switch scenario {
			case "expiry":
				expiry = expiry.Add(time.Minute)
			case "checkpoint":
				base.Sequence++
				child.Header.Generation.Graph.Sequence++
			case "epoch":
				base.Epoch = "11111111-1111-4111-8111-111111111111"
				child.Header.Generation.Graph.Epoch = base.Epoch
			case "chain":
				memoryBundlePGWrite(test, storage, ctx, child, expiry)
				identifier, base = child.ID, child.Header.Generation.Graph
				child.Header.Generation.Graph.Sequence++
				child.Header.Generation.Sequence++
			case "cross-repo":
				foreign := memorySourceRepoPG(test, ctx, storage, "foreign anchor")
				foreignAnchor := memoryBundlePGFixture(test, storage, ctx, foreign)
				memoryBundlePGWrite(test, storage, ctx, foreignAnchor, expiry)
				identifier, expected = foreignAnchor.ID, domain.ErrNotFound
			case "missing":
				identifier, expected = domain.HashContent([]byte("missing")), domain.ErrNotFound
			}
			for index := range child.Parts {
				part := &child.Parts[index]
				if part.Kind == "graph_ref" {
					part.Key, part.Sequence, part.Value = string(identifier), base.Sequence, memoryCaptureTestJSON(test, base)
				} else if strings.HasPrefix(part.Kind, "graph_delta:") {
					part.Sequence = child.Header.Generation.Graph.Sequence
				}
			}
			child = memoryBundlePGSeal(test, ctx, child.Header, child.Parts)
			before := memoryBundlePGCounts(test, storage, ctx, repo)
			if err := storage.WriteMemoryCapture(ctx, child, expiry); !errors.Is(err, expected) {
				test.Fatal("invalid graph base accepted", err)
			}
			if after := memoryBundlePGCounts(test, storage, ctx, repo); after != before {
				test.Fatal("rejection leaked rows", before, after)
			}
		})
	}
}

func TestPGMemoryCaptureGraphBaseDiscoveryNanosecondDeadline(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	anchor := memoryBundlePGFixture(test, storage, ctx, repo)
	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond).Add(987 * time.Nanosecond)
	memoryBundlePGWrite(test, storage, ctx, anchor, expiry)
	for _, minimum := range []time.Time{expiry, expiry.In(time.FixedZone("test", 9*60*60)), expiry.Truncate(time.Microsecond)} {
		identifier, err := storage.FindMemoryCaptureAnchor(ctx, anchor.Header.Generation.Graph, minimum)
		if err != nil || identifier != anchor.ID {
			test.Fatal("submicrosecond deadline rejected persisted anchor", minimum, identifier, err)
		}
	}
	if _, err := storage.FindMemoryCaptureAnchor(ctx, anchor.Header.Generation.Graph, expiry.Add(time.Microsecond)); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("later microsecond deadline accepted", err)
	}
}

func TestPGMemoryCaptureGraphBaseUnchangedRotationPreservesExpiry(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	anchor := memoryBundlePGFixture(test, storage, ctx, repo)
	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	requested := expiry.Add(time.Hour)
	memoryBundlePGWrite(test, storage, ctx, anchor, expiry)
	if _, err := storage.FindMemoryCaptureAnchor(ctx, anchor.Header.Generation.Graph, requested); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("short-lived anchor discovered", err)
	}
	rotated := memoryBundlePGSeal(test, ctx, anchor.Header, anchor.Parts)
	if rotated.ID != anchor.ID {
		test.Fatal("unchanged full capture changed immutable identity")
	}
	memoryBundlePGWrite(test, storage, ctx, rotated, requested)
	var retained time.Time
	if err := storage.pool.QueryRow(ctx, `SELECT expires_at FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(anchor.ID)).Scan(&retained); err != nil || !retained.Equal(expiry) {
		test.Fatal("same-ID full rotation extended TTL", retained, err)
	}
	if _, err := storage.FindMemoryCaptureAnchor(ctx, rotated.Header.Generation.Graph, requested); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("duplicate publication promised the requested lifetime", err)
	}
	memoryBundlePGRead(test, storage, ctx, rotated)
	memoryBundlePGExpire(test, storage, ctx, repo, anchor.ID)
	if err := storage.WriteMemoryCapture(ctx, rotated, requested); !errors.Is(err, domain.ErrMemoryCaptureExpired) {
		test.Fatal("expired same-ID publication resurrected retention", err)
	}
	if count, err := storage.PruneMemoryCaptures(ctx, repo, 1); err != nil || count != 1 {
		test.Fatal("expired receipt could not be pruned", count, err)
	}
	if _, err := storage.ReadMemoryCapture(ctx, repo, rotated.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("expired/pruned receipt remained usable", err)
	}
	test.Log("unchanged full rotation preserves the original expiry; requested later lifetime is not guaranteed, and expired retries require explicit prune/reprepare")
}

func TestPGMemoryCaptureGraphBaseDatabaseBinding(test *testing.T) {
	for _, scenario := range []string{"version", "epoch", "sequence", "reference", "checkpoint", "expiry", "chain", "cross-repo", "full-graph"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			anchor := memoryBundlePGFixture(test, storage, ctx, repo)
			expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
			memoryBundlePGWrite(test, storage, ctx, anchor, expiry)
			child := memoryGraphBasePGDelta(test, ctx, anchor, true)
			version, epoch, sequence, identifier := child.Header.Version, child.Header.Generation.Graph.Epoch, child.Header.Generation.Graph.Sequence, anchor.ID
			switch scenario {
			case "version":
				version, identifier = 1, ""
			case "epoch":
				epoch = "11111111-1111-4111-8111-111111111111"
			case "sequence":
				sequence++
			case "expiry":
				expiry = expiry.Add(time.Minute)
			case "chain":
				memoryBundlePGWrite(test, storage, ctx, child, expiry)
				identifier = child.ID
				child.Header.Generation.Sequence++
				child = memoryBundlePGSeal(test, ctx, child.Header, child.Parts)
			case "cross-repo":
				foreign := memorySourceRepoPG(test, ctx, storage, "foreign raw anchor")
				foreignAnchor := memoryBundlePGFixture(test, storage, ctx, foreign)
				memoryBundlePGWrite(test, storage, ctx, foreignAnchor, expiry)
				identifier = foreignAnchor.ID
			case "full-graph":
				child.Parts = append(child.Parts, domain.MemoryCapturePart{Kind: "graph:ref", Key: "forbidden", Value: []byte(`{}`)})
			case "reference", "checkpoint":
				for index := range child.Parts {
					if child.Parts[index].Kind == "graph_ref" {
						if scenario == "reference" {
							child.Parts[index].Key = string(domain.HashContent([]byte("incorrect anchor")))
						} else {
							checkpoint := anchor.Header.Generation.Graph
							checkpoint.Sequence++
							child.Parts[index].Value = memoryCaptureTestJSON(test, checkpoint)
						}
					}
				}
			}
			before := memoryBundlePGCounts(test, storage, ctx, repo)
			transaction := catalogTxPG(test, ctx, storage)
			err := memoryGraphBasePGInsert(ctx, transaction, child, expiry, version, epoch, sequence, identifier)
			if err == nil {
				err = transaction.Commit(ctx)
			}
			if err == nil {
				test.Fatal("database accepted invalid graph binding")
			}
			rollbackPG(transaction)
			if after := memoryBundlePGCounts(test, storage, ctx, repo); after != before {
				test.Fatal("failed binding leaked rows", before, after)
			}
		})
	}
}

func TestPGMemoryCaptureGraphBaseSealingAndConcurrentAppend(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	anchor := memoryBundlePGFixture(test, storage, ctx, repo)
	transaction := catalogTxPG(test, ctx, storage)
	body := []byte(`{}`)
	digest := domain.HashContent(body)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO memory_capture_blocks(repo_id,block_hash,body) VALUES($1,$2,$3)`, string(repo), string(digest), body)
	catalogExecPG(test, ctx, transaction, `INSERT INTO memory_capture_parts(repo_id,capture_id,ordinal,kind,entity_key,source_seq,block_hash)
		VALUES($1,$2,$3,'graph:ref','unindexed',0,$4)`, string(repo), string(anchor.ID), len(anchor.Parts), string(digest))
	memoryBundlePGWrite(test, storage, ctx, anchor, time.Now().Add(time.Hour))
	if err := transaction.Commit(ctx); err == nil {
		test.Fatal("concurrent unindexed append committed")
	}
	for _, query := range []string{
		`DELETE FROM memory_capture_parts WHERE repo_id=$1 AND capture_id=$2`,
		`INSERT INTO memory_capture_parts(repo_id,capture_id,ordinal,kind,entity_key,source_seq,block_hash)
		 SELECT repo_id,capture_id,1000,'graph:ref','appended',source_seq,block_hash FROM memory_capture_parts WHERE repo_id=$1 AND capture_id=$2 LIMIT 1`,
	} {
		if _, err := storage.pool.Exec(ctx, query, string(repo), string(anchor.ID)); err == nil {
			test.Fatal("sealed parts changed")
		}
	}
	memoryBundlePGRead(test, storage, ctx, anchor)
}

func TestPGMemoryCaptureGraphBasePruneLeavesRollbackAndRepositoryDelete(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	anchor := memoryBundlePGFixture(test, storage, ctx, repo)
	expiry := time.Now().Add(time.Hour)
	memoryBundlePGWrite(test, storage, ctx, anchor, expiry)
	first := memoryGraphBasePGDelta(test, ctx, anchor, false)
	unique := []byte("first child private memory")
	first.Parts = append(first.Parts, domain.MemoryCapturePart{Kind: "memory", Key: string(domain.HashContent(unique)) + ":00000000", Value: unique})
	first = memoryBundlePGSeal(test, ctx, first.Header, first.Parts)
	second := memoryGraphBasePGDelta(test, ctx, anchor, true)
	for _, child := range []domain.MemoryCaptureBundle{first, second} {
		memoryBundlePGWrite(test, storage, ctx, child, expiry)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(anchor.ID)); err == nil {
		test.Fatal("anchor deletion cascaded through children")
	}
	memoryBundlePGExpire(test, storage, ctx, repo, anchor.ID)
	if count, err := storage.PruneMemoryCaptures(ctx, repo, 32); err != nil || count != 0 {
		test.Fatal("expired anchor with retained children pruned", count, err)
	}
	for _, child := range []domain.MemoryCaptureBundle{first, second} {
		memoryBundlePGExpire(test, storage, ctx, repo, child.ID)
	}
	orphan := []byte("unrelated orphan must not be scanned")
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO memory_capture_blocks(repo_id,block_hash,body) VALUES($1,$2,$3)`, string(repo), string(domain.HashContent(orphan)), orphan)
	before := memoryBundlePGCounts(test, storage, ctx, repo)
	gateName := "graph_base_prune_" + strings.TrimPrefix(string(repo), "sha256:")[:12]
	catalogExecPG(test, ctx, storage.pool, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF OLD.repo_id='%s' THEN RAISE EXCEPTION 'injected cleanup failure'; END IF; RETURN OLD; END $$;
		CREATE TRIGGER %s BEFORE DELETE ON memory_capture_blocks FOR EACH ROW EXECUTE FUNCTION %s()`, gateName, repo, gateName, gateName))
	test.Cleanup(func() {
		_, _ = storage.pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON memory_capture_blocks; DROP FUNCTION IF EXISTS %s()`, gateName, gateName))
	})
	if count, err := storage.PruneMemoryCaptures(ctx, repo, 1); err == nil || count != 0 {
		test.Fatal("cleanup failure committed", count, err)
	}
	if after := memoryBundlePGCounts(test, storage, ctx, repo); after != before {
		test.Fatal("prune rollback lost headers or parts", before, after)
	}
	catalogExecPG(test, ctx, storage.pool, fmt.Sprintf(`DROP TRIGGER %s ON memory_capture_blocks; DROP FUNCTION %s()`, gateName, gateName))
	if count, err := storage.PruneMemoryCaptures(ctx, repo, 1); err != nil || count != 1 {
		test.Fatal("first leaf prune", count, err)
	}
	var anchorPresent bool
	if err := storage.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2)`, string(repo), string(anchor.ID)).Scan(&anchorPresent); err != nil || !anchorPresent {
		test.Fatal("leaf prune removed anchor", err)
	}
	storage.Close()
	var err error
	storage, err = NewPostgresStore(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	if count, err := storage.PruneMemoryCaptures(ctx, repo, 32); err != nil || count != 2 {
		test.Fatal("remaining leaf then anchor", count, err)
	}
	if counts := memoryBundlePGCounts(test, storage, ctx, repo); counts != [3]int{0, 0, 1} {
		test.Fatal("bounded cleanup scanned unrelated blocks or retained referenced ones", counts)
	}
	memoryBundlePGWrite(test, storage, ctx, anchor, expiry)
	memoryBundlePGWrite(test, storage, ctx, first, expiry)
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM repos WHERE id=$1`, string(repo))
	if counts := memoryBundlePGCounts(test, storage, ctx, repo); counts != [3]int{} {
		test.Fatal("same-repo no-action FK prevented repository cleanup", counts)
	}
}

type memoryGraphBasePGSize struct {
	rows      int64
	value     int64
	reference int64
	charged   int64
}

func memoryGraphBasePGMeasure(test *testing.T, storage *PostgresStore, ctx context.Context, repo, capture domain.ContentHash) memoryGraphBasePGSize {
	test.Helper()
	var size memoryGraphBasePGSize
	if err := storage.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(octet_length(block.body)),0),
		COALESCE(sum(octet_length(part.kind)+octet_length(part.entity_key)),0),
		COALESCE(sum(octet_length(block.body)::bigint+octet_length(part.kind)+octet_length(part.entity_key)+128),0)
		FROM memory_capture_parts part JOIN memory_capture_blocks block ON block.repo_id=part.repo_id AND block.block_hash=part.block_hash
		WHERE part.repo_id=$1 AND part.capture_id=$2 AND (part.kind LIKE 'graph:%' OR part.kind='graph_ref' OR part.kind LIKE 'graph_delta:%')`,
		string(repo), string(capture)).Scan(&size.rows, &size.value, &size.reference, &size.charged); err != nil {
		test.Fatal(err)
	}
	return size
}

func TestPGMemoryCaptureGraphBaseTenfoldPersistedCost(test *testing.T) {
	var baselineChild memoryGraphBasePGSize
	for _, graphRows := range []int{100, 1000} {
		test.Run(fmt.Sprint(graphRows), func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			anchor := memoryBundlePGFixture(test, storage, ctx, repo)
			for ordinal := 1; ordinal < graphRows; ordinal++ {
				anchor.Parts = append(anchor.Parts, domain.MemoryCapturePart{Kind: "graph:ref", Key: fmt.Sprintf("ref-%04d", ordinal),
					Sequence: anchor.Header.Generation.Graph.Sequence, Value: []byte(`{"value":"shared graph payload"}`)})
			}
			anchor = memoryBundlePGSeal(test, ctx, anchor.Header, anchor.Parts)
			expiry := time.Now().Add(time.Hour)
			memoryBundlePGWrite(test, storage, ctx, anchor, expiry)
			child := memoryGraphBasePGDelta(test, ctx, anchor, true)
			memoryBundlePGWrite(test, storage, ctx, child, expiry)
			baseSize := memoryGraphBasePGMeasure(test, storage, ctx, repo, anchor.ID)
			childSize := memoryGraphBasePGMeasure(test, storage, ctx, repo, child.ID)
			if baseSize.rows != int64(graphRows) || childSize.rows != 3 {
				test.Fatal("unexpected persisted graph rows", baseSize, childSize)
			}
			if graphRows == 100 {
				baselineChild = childSize
			} else if childSize != baselineChild {
				test.Fatal("fixed delta storage grew with graph", baselineChild, childSize)
			}
			test.Logf("persisted graph inputs: anchor rows=%d value_bytes=%d kind_key_bytes=%d charged_bytes=%d; child rows=%d value_bytes=%d kind_key_bytes=%d charged_bytes=%d (one ref, one upsert, one tombstone)",
				baseSize.rows, baseSize.value, baseSize.reference, baseSize.charged, childSize.rows, childSize.value, childSize.reference, childSize.charged)
		})
	}
}
