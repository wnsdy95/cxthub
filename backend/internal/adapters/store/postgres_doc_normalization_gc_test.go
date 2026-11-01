//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func legacyNormalizationGCPGRoot(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob, expected legacyByteCheckpoint, prefix legacyNormalizationPrefix) legacyByteCheckpoint {
	test.Helper()
	body, err := json.Marshal(struct {
		Version uint8                     `json:"v"`
		State   map[string]int            `json:"s"`
		Header  legacyNormalizationPrefix `json:"h"`
	}{Version: 1, State: map[string]int{"Version": 1}, Header: prefix})
	if err != nil {
		test.Fatal(err)
	}
	object := legacyByteCheckpointObject{hash: domain.HashContent(body), data: body, structure: 2}
	state, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, expected, object.hash, len(body), []legacyByteCheckpointObject{object})
	if err != nil {
		test.Fatal(err)
	}
	return state
}

func legacyNormalizationGCPGDrain(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob, state legacyByteCheckpoint) int64 {
	test.Helper()
	var removed int64
	for quantum := 0; quantum < 10000; quantum++ {
		complete, count, err := storage.reclaimLegacyNormalizationCheckpoint(ctx, job, state)
		if err != nil || count > 128 {
			test.Fatal("bounded current-root reclamation failed", quantum, count, err)
		}
		removed += count
		if complete {
			return removed
		}
	}
	test.Fatal("bounded checkpoint reclamation did not terminate")
	return 0
}

func TestLegacyNormalizationGCPGPreservesCurrentRootAndPendingReferences(test *testing.T) {
	storage, ctx, _, job, initial := legacyCheckpointPGStart(test)
	const chunk = domain.LegacyCanonicalEncodingBytes
	body := bytes.Repeat([]byte("a"), 3*chunk)
	var prefix legacyNormalizationPrefix
	var err error
	for prefix.Tree.DataBytes < len(body) {
		prefix, err = storage.stageLegacyNormalizationPrefix(ctx, job, initial, prefix, 1, chunk, len(body),
			func(ctx context.Context, offset, length int) ([]byte, error) {
				return body[offset : offset+length], ctx.Err()
			})
		if err != nil {
			test.Fatal(err)
		}
	}
	state := legacyNormalizationGCPGRoot(test, storage, ctx, job, initial, prefix)
	garbage := legacyCheckpointPGObject("pending-for-next-root")
	if err := storage.stageLegacyNormalizationCheckpointObjects(ctx, job, state, []legacyByteCheckpointObject{garbage}); err != nil {
		test.Fatal(err)
	}
	if removed := legacyNormalizationGCPGDrain(test, storage, ctx, job, state); removed == 0 {
		test.Fatal("old unreferenced index versions were not reclaimed")
	}
	if data, err := storage.readLegacyNormalizationPrefix(ctx, job, state, prefix, chunk-10, 20); err != nil || !bytes.Equal(data, body[chunk-10:chunk+10]) {
		test.Fatal("reclamation damaged live prefix data or its index", err)
	}
	if _, err := storage.readLegacyNormalizationCheckpointObject(ctx, job, state, garbage.hash, len(garbage.data)); err != nil {
		test.Fatal("reclamation deleted a current-sequence pending dependency", err)
	}
	if _, _, err := storage.reclaimLegacyNormalizationCheckpoint(ctx, job, initial); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("empty root was accepted for reclamation", err)
	}
	newRoot := legacyNormalizationGCPGRoot(test, storage, ctx, job, state, prefix)
	legacyNormalizationGCPGDrain(test, storage, ctx, job, newRoot)
	if _, err := storage.readLegacyNormalizationCheckpointObject(ctx, job, newRoot, garbage.hash, len(garbage.data)); !errors.Is(err, domain.ErrPreparedDocRangeObject) {
		test.Fatal("abandoned prior-sequence dependency remained reachable", err)
	}
	if _, _, err := storage.reclaimLegacyNormalizationCheckpoint(ctx, job, state); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("old root reclaimed through a newer committed root", err)
	}
}

func TestLegacyNormalizationGCPGConstraintsAndDiscard(test *testing.T) {
	storage, ctx, _, job, initial := legacyCheckpointPGStart(test)
	prefix, err := storage.stageLegacyNormalizationPrefix(ctx, job, initial, legacyNormalizationPrefix{}, 1, domain.LegacyCanonicalEncodingBytes, 4,
		func(ctx context.Context, offset, length int) ([]byte, error) { return []byte("live"), ctx.Err() })
	if err != nil {
		test.Fatal(err)
	}
	state := legacyNormalizationGCPGRoot(test, storage, ctx, job, initial, prefix)
	for _, statement := range []string{
		`DELETE FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1 AND hash=$2`,
		`DELETE FROM doc_legacy_checkpoint_edges_v1 WHERE doc_hash=$1`,
		`DELETE FROM doc_legacy_checkpoint_structures_v1 WHERE doc_hash=$1`,
	} {
		transaction, err := storage.pool.Begin(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, `SET LOCAL cxt.legacy_byte_checkpoint_protocol='1';SET LOCAL cxt.legacy_checkpoint_gc_protocol='1'`); err != nil {
			test.Fatal(err)
		}
		if statement == `DELETE FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1 AND hash=$2` {
			_, err = transaction.Exec(ctx, statement, job.DocHash, prefix.Tree.Hash)
		} else {
			_, err = transaction.Exec(ctx, statement, job.DocHash)
		}
		rollbackPG(transaction)
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "23514" {
			test.Fatal("current-root reference metadata was not immutable/pinned", statement, err)
		}
	}
	if err := storage.discardLegacyNormalizationCheckpoint(ctx, job, state); err != nil {
		test.Fatal("explicit current-root discard did not cascade reference metadata", err)
	}
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_legacy_checkpoint_staging_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_checkpoint_structures_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_checkpoint_edges_v1 WHERE doc_hash=$1)`, job.DocHash).Scan(&count); err != nil || count != 0 {
		test.Fatal("discard left reference/staging rows behind", count, err)
	}
}

func legacyNormalizationGCPGImage(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob) string {
	test.Helper()
	var image string
	err := storage.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'stage',(SELECT to_jsonb(stage) FROM doc_legacy_byte_stages_v1 stage WHERE doc_hash=$1),
 'objects',(SELECT jsonb_agg(to_jsonb(object) ORDER BY hash) FROM doc_legacy_byte_stage_checkpoint_objects_v1 object WHERE doc_hash=$1),
 'staging',(SELECT jsonb_agg(to_jsonb(staging) ORDER BY hash) FROM doc_legacy_checkpoint_staging_v1 staging WHERE doc_hash=$1),
 'structures',(SELECT jsonb_agg(to_jsonb(structure) ORDER BY hash) FROM doc_legacy_checkpoint_structures_v1 structure WHERE doc_hash=$1),
 'edges',(SELECT jsonb_agg(to_jsonb(edge) ORDER BY parent_hash,child_hash) FROM doc_legacy_checkpoint_edges_v1 edge WHERE doc_hash=$1),
 'usage',(SELECT to_jsonb(usage) FROM doc_legacy_byte_usage_v1 usage WHERE doc_hash=$1),
 'job',(SELECT to_jsonb(job) FROM doc_range_jobs_v1 job WHERE repo_id=$2 AND doc_hash=$1))::text`,
		job.DocHash, job.RepoID).Scan(&image)
	if err != nil {
		test.Fatal(err)
	}
	return image
}

func TestLegacyNormalizationGCPGDeduplicatedReusePinsCurrentSequence(test *testing.T) {
	for _, reuse := range []string{"deduplicated-write", "unchanged-prefix"} {
		test.Run(reuse, func(test *testing.T) {
			storage, ctx, _, job, initial := legacyCheckpointPGStart(test)
			body := []byte("old-unreferenced-prefix-reused-before-next-root")
			read := func(ctx context.Context, offset, length int) ([]byte, error) {
				return body[offset : offset+length], ctx.Err()
			}
			prefix, err := storage.stageLegacyNormalizationPrefix(ctx, job, initial, legacyNormalizationPrefix{}, 1,
				domain.LegacyCanonicalEncodingBytes, len(body), read)
			if err != nil {
				test.Fatal(err)
			}
			state := legacyNormalizationGCPGRoot(test, storage, ctx, job, initial, legacyNormalizationPrefix{})
			before := legacyByteUsagePGRead(test, storage, ctx)
			previous := legacyNormalizationPrefix{}
			if reuse == "unchanged-prefix" {
				previous = prefix
			}
			current, err := storage.stageLegacyNormalizationPrefix(ctx, job, state, previous, 1,
				domain.LegacyCanonicalEncodingBytes, len(body), read)
			if err != nil || current != prefix || legacyByteUsagePGRead(test, storage, ctx) != before {
				test.Fatal("reuse changed immutable identity or duplicated accounting", current, err)
			}
			legacyNormalizationGCPGDrain(test, storage, ctx, job, state)
			var sequence int64
			if err := storage.pool.QueryRow(ctx, `SELECT last_sequence FROM doc_legacy_checkpoint_staging_v1
 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`, job.DocHash, state.stage.incarnation, current.Tree.Hash).Scan(&sequence); err != nil || sequence != state.sequence {
				test.Fatal("deduplicated pending dependency lost its sequence pin", sequence, err)
			}
			if data, err := storage.readLegacyNormalizationPrefix(ctx, job, state, current, 0, len(body)); err != nil || !bytes.Equal(data, body) {
				test.Fatal("reclamation deleted a reused pending dependency", err)
			}
		})
	}
}

func TestLegacyNormalizationGCPGRejectsReplacedClaimAndStage(test *testing.T) {
	for _, replacement := range []string{"claim", "stage"} {
		test.Run(replacement, func(test *testing.T) {
			storage, ctx, fixture, job, initial := legacyCheckpointPGStart(test)
			old := legacyNormalizationGCPGRoot(test, storage, ctx, job, initial, legacyNormalizationPrefix{})
			currentJob, current := job, old
			if replacement == "claim" {
				finished := job
				finished.State, finished.Reason, finished.NextAttempt = "retrying", "GC worker replacement", time.Now().UTC().Add(-time.Minute)
				if err := storage.FinishDocRangeJob(ctx, finished, time.Now().UTC()); err != nil {
					test.Fatal(err)
				}
				currentJob = docRangeJobPGClaim(test, storage, ctx, fixture.repo, fixture.hash)
			} else {
				if _, err := storage.pool.Exec(ctx, `DELETE FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, job.DocHash); err != nil {
					test.Fatal(err)
				}
				source := old.stage
				source.incarnation = ""
				if _, err := storage.ensureLegacyNormalizationStage(ctx, job, source); err != nil {
					test.Fatal(err)
				}
				fresh, _, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
				if err != nil {
					test.Fatal(err)
				}
				current = legacyNormalizationGCPGRoot(test, storage, ctx, job, fresh, legacyNormalizationPrefix{})
				if current.hash != old.hash || current.sequence != old.sequence || current.stage.incarnation == old.stage.incarnation {
					test.Fatal("stage ABA fixture did not preserve root/hash and replace incarnation")
				}
			}
			before := legacyNormalizationGCPGImage(test, storage, ctx, currentJob)
			if _, _, err := storage.reclaimLegacyNormalizationCheckpoint(ctx, job, old); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("replaced GC actor was not rejected", err)
			}
			if legacyNormalizationGCPGImage(test, storage, ctx, currentJob) != before {
				test.Fatal("rejected GC actor changed objects, usage, cursor, or current job")
			}
			legacyNormalizationGCPGDrain(test, storage, ctx, currentJob, current)
		})
	}
}

func TestLegacyNormalizationGCPGResumesPersistedCursorAcrossClaim(test *testing.T) {
	storage, ctx, fixture, job, initial := legacyCheckpointPGStart(test)
	child := legacyCheckpointPGObject("child-observed-before-orphan-parent")
	var parent legacyByteCheckpointObject
	for attempt := 0; attempt < 100; attempt++ {
		body, err := json.Marshal(legacyNormalizationPrefixNode{Version: 1, Children: []legacyNormalizationPrefixTree{
			{Hash: child.hash, Bytes: len(child.data), DataBytes: len(child.data), Leaves: 1},
			{Hash: domain.HashContent([]byte(fmt.Sprint(attempt))), Bytes: len(fmt.Sprint(attempt)), DataBytes: len(fmt.Sprint(attempt)), Leaves: 1},
		}})
		if err != nil {
			test.Fatal(err)
		}
		parent = legacyByteCheckpointObject{hash: domain.HashContent(body), data: body, structure: 1}
		if child.hash < parent.hash {
			extra := legacyCheckpointPGObject(fmt.Sprint(attempt))
			if err := storage.stageLegacyNormalizationCheckpointObjects(ctx, job, initial, []legacyByteCheckpointObject{child, extra, parent}); err != nil {
				test.Fatal(err)
			}
			break
		}
	}
	if child.hash >= parent.hash {
		test.Fatal("fixture failed to order child before parent")
	}
	for first := 0; first < 200; first += 8 {
		objects := make([]legacyByteCheckpointObject, 0, 8)
		for ordinal := first; ordinal < first+8; ordinal++ {
			objects = append(objects, legacyCheckpointPGObject(fmt.Sprintf("garbage-%03d", ordinal)))
		}
		if err := storage.stageLegacyNormalizationCheckpointObjects(ctx, job, initial, objects); err != nil {
			test.Fatal(err)
		}
	}
	state := legacyNormalizationGCPGRoot(test, storage, ctx, job, initial, legacyNormalizationPrefix{})
	complete, count, err := storage.reclaimLegacyNormalizationCheckpoint(ctx, job, state)
	if err != nil || complete || count > 128 || count == 0 {
		test.Fatal("first bounded batch did not persist an incomplete pass", complete, count, err)
	}
	var cursor domain.ContentHash
	var sequence int64
	if err := storage.pool.QueryRow(ctx, `SELECT checkpoint_gc_last_hash,checkpoint_gc_sequence FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, job.DocHash).
		Scan(&cursor, &sequence); err != nil || cursor == "" || sequence != state.sequence {
		test.Fatal("first committed batch lost its continuation", cursor, sequence, err)
	}
	finished := job
	finished.State, finished.Reason, finished.NextAttempt = "retrying", "resume bounded GC", time.Now().UTC().Add(-time.Minute)
	if err := storage.FinishDocRangeJob(ctx, finished, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	current := docRangeJobPGClaim(test, storage, ctx, fixture.repo, fixture.hash)
	loaded, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, current)
	if err != nil || !found || loaded != state || current.Version == job.Version {
		test.Fatal("new actor did not retain the committed root", loaded, current, err)
	}
	complete, count, err = storage.reclaimLegacyNormalizationCheckpoint(ctx, current, loaded)
	if err != nil || complete || count > 128 {
		test.Fatal("new actor failed to continue the persisted pass", complete, count, err)
	}
	var next domain.ContentHash
	if err := storage.pool.QueryRow(ctx, `SELECT checkpoint_gc_last_hash FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, job.DocHash).Scan(&next); err != nil || next <= cursor {
		test.Fatal("new actor restarted rather than continuing its saved keyset", cursor, next, err)
	}
	legacyNormalizationGCPGDrain(test, storage, ctx, current, loaded)
	usage := legacyByteUsagePGRead(test, storage, ctx)
	if usage.CheckpointObjects != 1 || usage.CheckpointBytes != int64(loaded.bytes) {
		test.Fatal("repeated bounded passes left orphan parents or their children", usage)
	}
}

func TestLegacyNormalizationGCPGFinalFenceRollsBackReclamation(test *testing.T) {
	storage, ctx, fixture, job, initial := legacyCheckpointPGStart(test)
	prefix, err := storage.stageLegacyNormalizationPrefix(ctx, job, initial, legacyNormalizationPrefix{}, 1,
		domain.LegacyCanonicalEncodingBytes, 4, func(ctx context.Context, offset, length int) ([]byte, error) { return []byte("gone"), ctx.Err() })
	if err != nil {
		test.Fatal(err)
	}
	legacyNormalizationGCPGRoot(test, storage, ctx, job, initial, prefix)
	loaded, _, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	state := legacyNormalizationGCPGRoot(test, storage, ctx, job, loaded, legacyNormalizationPrefix{})
	name := pgx.Identifier{domain.NewID("checkpoint_gc_final_")}.Sanitize()
	statement := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.doc_hash='%s' AND NEW.checkpoint_gc_sequence<>OLD.checkpoint_gc_sequence THEN
 UPDATE doc_range_jobs_v1 SET state='waiting',version=version+1,lease_until=NULL WHERE repo_id='%s' AND doc_hash=NEW.doc_hash;
 END IF; RETURN NEW; END; $$;
 CREATE TRIGGER %s AFTER UPDATE ON doc_legacy_byte_stages_v1 FOR EACH ROW EXECUTE FUNCTION %s()`, name, fixture.hash, fixture.repo, name, name)
	if _, err := storage.pool.Exec(ctx, statement); err != nil {
		test.Fatal(err)
	}
	defer func() {
		if _, err := storage.pool.Exec(context.WithoutCancel(ctx), `DROP TRIGGER IF EXISTS `+name+` ON doc_legacy_byte_stages_v1; DROP FUNCTION IF EXISTS `+name+`() `); err != nil {
			test.Error(err)
		}
	}()
	before := legacyNormalizationGCPGImage(test, storage, ctx, job)
	if _, _, err := storage.reclaimLegacyNormalizationCheckpoint(ctx, job, state); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("GC committed after losing its final claim fence", err)
	}
	if legacyNormalizationGCPGImage(test, storage, ctx, job) != before {
		test.Fatal("failed GC changed objects, cascading references, usage, cursor, or job")
	}
}
