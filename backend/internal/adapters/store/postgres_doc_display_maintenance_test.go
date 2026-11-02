//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func maintenanceSource(test *testing.T, ctx context.Context, storage *PostgresStore, label string) (domain.ContentHash, domain.ContentHash) {
	test.Helper()
	event := domain.HashContent([]byte(label))
	source := domain.DocEventDisplaySource{Version: 1, Profile: domain.DocDisplayProjectionProfile, EventHash: event, EventBytes: 100, Kind: domain.EventReasoning}
	raw, err := source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	hash := domain.HashContent(raw)
	if _, err := storage.pool.Exec(ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes) VALUES($1,$2,$3,$4)`, event, source.Profile, hash, raw); err != nil {
		test.Fatal(err)
	}
	return hash, event
}

func maintenanceObject(test *testing.T, value any) domain.DocRangeObject {
	test.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	return domain.DocRangeObject{Hash: domain.HashContent(raw), Bytes: raw}
}

func maintenancePut(test *testing.T, ctx context.Context, storage *PostgresStore, source domain.ContentHash, objects ...domain.DocRangeObject) {
	test.Helper()
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if err := putDocDisplayObjects(ctx, transaction, source, objects); err != nil {
		test.Fatal(err)
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
}

func maintenanceRoot(test *testing.T, ctx context.Context, storage *PostgresStore, source, event, root domain.ContentHash) {
	test.Helper()
	raw, _ := json.Marshal(map[string]any{"plan": root})
	_, err := storage.pool.Exec(ctx, `INSERT INTO doc_display_generations_v1(event_hash,profile,source_hash,generation_hash,bytes)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT(event_hash,profile) DO UPDATE SET generation_hash=EXCLUDED.generation_hash,bytes=EXCLUDED.bytes`, event, domain.DocDisplayProjectionProfile, source, domain.HashContent(raw), raw)
	if err != nil {
		test.Fatal(err)
	}
}

func drainDisplayMaintenance(test *testing.T, ctx context.Context, storage *PostgresStore) (int, int64) {
	test.Helper()
	deleted := 0
	var deletedBytes int64
	for count := 0; count < 2000; count++ {
		step, err := storage.StepDocDisplayMaintenance(ctx)
		if errors.Is(err, domain.ErrNotFound) {
			return deleted, deletedBytes
		}
		if err != nil {
			test.Fatal("maintenance step", count, err)
		}
		if step.Indexed > 8 || step.Marked > domain.DocDisplayMaintenanceBatch || step.Deleted > domain.DocDisplayMaintenanceBatch {
			test.Fatal("unbounded maintenance step", step)
		}
		deleted += step.Deleted
		deletedBytes += step.DeletedBytes
	}
	test.Fatal("maintenance failed to converge")
	return 0, 0
}

func TestDocDisplayMaintenancePGGraphRevisionAndScope(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	source, event := maintenanceSource(test, ctx, storage, "revision source")
	foreign, foreignEvent := maintenanceSource(test, ctx, storage, "foreign source")
	leaf := maintenanceObject(test, map[string]any{"text": "kept"})
	parent := maintenanceObject(test, map[string]any{"child": leaf.Hash, "digest": domain.HashContent([]byte("absent digest"))})
	old := maintenanceObject(test, map[string]any{"old": "now unreachable"})
	unused := maintenanceObject(test, map[string]any{"unused": true})
	maintenancePut(test, ctx, storage, source, leaf, parent, old, unused)
	maintenancePut(test, ctx, storage, foreign, unused)
	maintenanceRoot(test, ctx, storage, source, event, old.Hash)
	maintenanceRoot(test, ctx, storage, foreign, foreignEvent, unused.Hash)
	for count := 0; count < 100; count++ {
		step, err := storage.StepDocDisplayMaintenance(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if step.Source == source && step.Phase == "sweep" {
			break
		}
		if count == 99 {
			test.Fatal("did not reach revision boundary")
		}
	}
	maintenanceRoot(test, ctx, storage, source, event, parent.Hash)
	drainDisplayMaintenance(test, ctx, storage)
	for _, hash := range []domain.ContentHash{leaf.Hash, parent.Hash} {
		if _, err := storage.loadDocDisplayObject(ctx, source, hash); err != nil {
			test.Fatal("newly rooted dependency was reclaimed", hash, err)
		}
	}
	if _, err := storage.loadDocDisplayObject(ctx, foreign, unused.Hash); err != nil {
		test.Fatal("source-local cleanup crossed foreign scope", err)
	}
	if _, err := storage.pool.Exec(ctx, `SELECT cxthub_queue_doc_display_maintenance($1)`, source); err != nil {
		test.Fatal(err)
	}
	drainDisplayMaintenance(test, ctx, storage)
	for _, hash := range []domain.ContentHash{old.Hash, unused.Hash} {
		var count int
		if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2`, source, hash).Scan(&count); err != nil || count != 0 {
			test.Fatal("obsolete object was not reclaimed", hash, count, err)
		}
	}
	if _, err := storage.StepDocDisplayMaintenance(outbound.WithDocReadOnly(ctx)); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("read-only scope reclaimed data", err)
	}
}

func TestDocDisplayMaintenancePGBackfillAndUnknownEncoding(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	source, event := maintenanceSource(test, ctx, storage, "legacy backfill")
	leaf := maintenanceObject(test, map[string]any{"text": "legacy leaf"})
	parent := maintenanceObject(test, map[string]any{"child": leaf.Hash})
	unused := maintenanceObject(test, map[string]any{"old": "legacy orphan"})
	for _, object := range []domain.DocRangeObject{leaf, parent, unused} {
		if _, err := storage.pool.Exec(ctx, `INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) VALUES($1,$2,$3)`, source, object.Hash, object.Bytes); err != nil {
			test.Fatal(err)
		}
	}
	maintenanceRoot(test, ctx, storage, source, event, parent.Hash)
	deleted, _ := drainDisplayMaintenance(test, ctx, storage)
	if deleted != 1 {
		test.Fatal("legacy indexing reclaimed the wrong objects", deleted)
	}
	if _, err := storage.loadDocDisplayObject(ctx, source, leaf.Hash); err != nil {
		test.Fatal(err)
	}
	poison, _ := maintenanceSource(test, ctx, storage, "unknown encoding")
	raw := []byte{0, 1, 2}
	if _, err := storage.pool.Exec(ctx, `INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) VALUES($1,$2,$3)`, poison, domain.HashContent(raw), raw); err != nil {
		test.Fatal(err)
	}
	var maintenanceErr error
	for count := 0; count < 4; count++ {
		step, err := storage.StepDocDisplayMaintenance(ctx)
		maintenanceErr = err
		if err != nil || step.Phase != "catalog_census" {
			break
		}
	}
	if !errors.Is(maintenanceErr, domain.ErrDocDisplayReferencesUnsupported) {
		test.Fatal("unknown bytes acquired a reference-free witness", maintenanceErr)
	}
	var future bool
	if err := storage.pool.QueryRow(ctx, `SELECT due_at>clock_timestamp() FROM doc_display_maintenance_v1 WHERE source_hash=$1`, poison).Scan(&future); err != nil || !future {
		test.Fatal("unsupported source could starve other sources", future, err)
	}
	if _, err := storage.loadDocDisplayObject(ctx, poison, domain.HashContent(raw)); err != nil {
		test.Fatal("unknown object was deleted", err)
	}
}

func TestDocDisplayMaintenancePGLegacyInsertFencesBeforeReplay(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	source, _ := maintenanceSource(test, ctx, storage, "old writer fence")
	object := maintenanceObject(test, map[string]any{"text": "old writer replay"})
	maintenancePut(test, ctx, storage, source, object)
	locker, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(locker)
	var revision int64
	if err := locker.QueryRow(ctx, `SELECT reference_revision FROM doc_display_sources_v1 WHERE source_hash=$1 FOR NO KEY UPDATE`, source).Scan(&revision); err != nil {
		test.Fatal(err)
	}
	writer, err := storage.pool.Acquire(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer writer.Release()
	var pid int
	if err := writer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		test.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := writer.Exec(ctx, `INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, source, object.Hash, object.Bytes)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		if err := storage.pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked); err != nil {
			test.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !blocked {
		test.Fatal("old replay did not wait on source fence")
	}
	if err := locker.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			test.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("old replay deadlocked on foreign-key upgrade")
	}
	var after int64
	if err := storage.pool.QueryRow(ctx, `SELECT reference_revision FROM doc_display_sources_v1 WHERE source_hash=$1`, source).Scan(&after); err != nil || after <= revision {
		test.Fatal("old replay failed to revise source", after, revision, err)
	}
}

func TestDocDisplayMaintenancePGSyntheticProfilesAtRootBatchBoundary(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	repo, _ := rootPGRepo(test, ctx, storage)
	source, event := maintenanceSource(test, ctx, storage, "synthetic cross-profile root enumeration")
	first := maintenanceObject(test, map[string]any{"text": "first profile"})
	last := maintenanceObject(test, map[string]any{"text": "last profile"})
	maintenancePut(test, ctx, storage, source, first, last)
	for ordinal := 0; ordinal < 9; ordinal++ {
		profile := domain.DocDisplayProjectionProfile
		protocol, index := 5, ordinal
		var checkpointHash, checkpoint, preparationHash, preparation any
		raw, err := json.Marshal(map[string]any{"root": first.Hash})
		if err != nil {
			test.Fatal(err)
		}
		preparationHash, preparation = domain.HashContent(raw), raw
		if ordinal == 8 {
			profile, protocol, index = domain.DocDisplayStringProfile, 2, 7
			raw, err = json.Marshal(map[string]any{"root": last.Hash})
			if err != nil {
				test.Fatal(err)
			}
			checkpointHash, checkpoint = domain.HashContent(raw), raw
			preparationHash, preparation = nil, nil
		}
		_, err = docDisplayJobCorruptionPG(ctx, storage, `INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,profile,event_index,event_hash,event_offset,event_length,event_seq,event_role,source_id,source_version,range_generation,state,created_at,next_attempt,source_hash,checkpoint_hash,checkpoint,preparation_hash,preparation,worker_protocol,catalog_epoch)
 VALUES($1,$2,$3,$4,$5,0,100,0,'',gen_random_uuid(),0,$2,'waiting',clock_timestamp(),clock_timestamp(),$6,$7,$8,$9,$10,$11,
 CASE WHEN $3='public-event-display-v1' THEN (SELECT catalog_epoch FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=$6) END)`,
			repo, domain.HashContent([]byte("synthetic root enumeration document")), profile, index, event, source, checkpointHash, checkpoint, preparationHash, preparation, protocol)
		if err != nil {
			test.Fatal("synthetic root row", ordinal, err)
		}
	}
	test.Log("Synthetic hash-consistent roots exercise complete primary-key pagination; they are not typed published event checkpoints.")
	deleted, _ := drainDisplayMaintenance(test, ctx, storage)
	if deleted != 0 {
		test.Fatal("profile at the next batch was skipped", deleted)
	}
	if _, err := storage.loadDocDisplayObject(ctx, source, last.Hash); err != nil {
		test.Fatal("last profile root was reclaimed", err)
	}
}

func TestDocDisplayMaintenancePGConcurrentCollectors(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	source, event := maintenanceSource(test, ctx, storage, "concurrent collectors")
	leaf := maintenanceObject(test, map[string]any{"text": "retained leaf"})
	root := maintenanceObject(test, map[string]any{"child": leaf.Hash})
	objects := []domain.DocRangeObject{leaf, root}
	for ordinal := 0; ordinal < 150; ordinal++ {
		objects = append(objects, maintenanceObject(test, map[string]any{"obsolete": ordinal}))
	}
	maintenancePut(test, ctx, storage, source, objects...)
	maintenanceRoot(test, ctx, storage, source, event, root.Hash)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			for range 64 {
				_, err := storage.StepDocDisplayMaintenance(ctx)
				if errors.Is(err, domain.ErrNotFound) {
					continue
				}
				if err != nil {
					results <- err
					return
				}
			}
			results <- nil
		})
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			test.Fatal("competing collectors", err)
		}
	}
	drainDisplayMaintenance(test, ctx, storage)
	var retained int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_display_objects_v1 WHERE source_hash=$1`, source).Scan(&retained); err != nil || retained != 2 {
		test.Fatal("concurrent maintenance did not converge to the live graph", retained, err)
	}
	if _, err := storage.loadDocDisplayObject(ctx, source, leaf.Hash); err != nil {
		test.Fatal("competing sweeps reclaimed live leaf", err)
	}
}
