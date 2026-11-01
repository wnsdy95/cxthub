//go:build postgres

package store

import (
	"bytes"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocDisplayMaintenancePGLateIndexedChildSurvivesRevisionRestart(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	source, event := maintenanceSource(test, ctx, storage, "late indexed child")
	child := maintenanceObject(test, map[string]any{"text": "late child"})
	parent := maintenanceObject(test, map[string]any{"child": child.Hash})
	maintenancePut(test, ctx, storage, source, parent)
	maintenanceRoot(test, ctx, storage, source, event, parent.Hash)
	for count := 0; count < 100; count++ {
		step, err := storage.StepDocDisplayMaintenance(ctx)
		if err != nil {
			test.Fatal("advance to pre-sweep boundary", count, err)
		}
		if step.Source == source && step.Phase == "sweep" {
			break
		}
		if count == 99 {
			test.Fatal("did not reach pre-sweep boundary")
		}
	}
	var initialEpoch int64
	var parentExpanded, childPresent, edgePresent bool
	if err := storage.pool.QueryRow(ctx, `SELECT work.epoch,mark.expanded,
 EXISTS(SELECT 1 FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$3),
 EXISTS(SELECT 1 FROM doc_display_object_edges_v1 WHERE source_hash=$1 AND parent_hash=$2 AND child_hash=$3)
 FROM doc_display_maintenance_v1 work JOIN doc_display_object_marks_v1 mark
 ON mark.source_hash=work.source_hash AND mark.epoch=work.epoch AND mark.hash=$2
 WHERE work.source_hash=$1`, source, parent.Hash, child.Hash).Scan(&initialEpoch, &parentExpanded, &childPresent, &edgePresent); err != nil {
		test.Fatal(err)
	}
	if initialEpoch <= 0 || !parentExpanded || childPresent || !edgePresent {
		test.Fatal("fixture lacks an expanded parent with an absent candidate child", initialEpoch, parentExpanded, childPresent, edgePresent)
	}
	maintenancePut(test, ctx, storage, source, child)
	var phase string
	var publicationEpoch int64
	var revisionChanged, unindexed bool
	var coverage int16
	if err := storage.pool.QueryRow(ctx, `SELECT work.phase,work.epoch,source.reference_revision>work.source_revision,
 object.references_version,EXISTS(SELECT 1 FROM doc_display_objects_v1 WHERE source_hash=$1 AND references_version=0)
 FROM doc_display_maintenance_v1 work JOIN doc_display_sources_v1 source USING(source_hash)
 JOIN doc_display_objects_v1 object ON object.source_hash=work.source_hash AND object.hash=$2
 WHERE work.source_hash=$1`, source, child.Hash).Scan(&phase, &publicationEpoch, &revisionChanged, &coverage, &unindexed); err != nil {
		test.Fatal(err)
	}
	if phase != "sweep" || publicationEpoch != initialEpoch || !revisionChanged || coverage != 1 || unindexed {
		test.Fatal("late publication masked the active-epoch regression", phase, publicationEpoch, initialEpoch, revisionChanged, coverage, unindexed)
	}
	deleted, _ := drainDisplayMaintenance(test, ctx, storage)
	if deleted != 0 {
		test.Fatal("reachable objects were reclaimed after the revision restart", deleted)
	}
	for _, object := range []domain.DocRangeObject{parent, child} {
		raw, err := storage.loadDocDisplayObject(ctx, source, object.Hash)
		if err != nil || !bytes.Equal(raw, object.Bytes) {
			test.Fatal("reachable object changed or disappeared", object.Hash, err)
		}
	}
	var finalEpoch int64
	var childExpanded bool
	if err := storage.pool.QueryRow(ctx, `SELECT work.phase,work.epoch,mark.expanded
 FROM doc_display_maintenance_v1 work JOIN doc_display_object_marks_v1 mark
 ON mark.source_hash=work.source_hash AND mark.epoch=work.epoch AND mark.hash=$2
 WHERE work.source_hash=$1`, source, child.Hash).Scan(&phase, &finalEpoch, &childExpanded); err != nil {
		test.Fatal(err)
	}
	if phase != "idle" || finalEpoch <= initialEpoch || !childExpanded {
		test.Fatal("revision restart failed to complete a fresh marking epoch", phase, finalEpoch, initialEpoch, childExpanded)
	}
}

func TestDocDisplayMaintenancePGGenerationHashMismatchPreventsDeletion(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	source, event := maintenanceSource(test, ctx, storage, "tampered generation proof")
	child := maintenanceObject(test, map[string]any{"text": "rooted child"})
	parent := maintenanceObject(test, map[string]any{"child": child.Hash})
	orphan := maintenanceObject(test, map[string]any{"text": "unreachable object"})
	objects := []domain.DocRangeObject{child, parent, orphan}
	maintenancePut(test, ctx, storage, source, objects...)
	maintenanceRoot(test, ctx, storage, source, event, parent.Hash)
	var generationHash domain.ContentHash
	var generationRaw []byte
	if err := storage.pool.QueryRow(ctx, `SELECT generation_hash,bytes FROM doc_display_generations_v1
 WHERE source_hash=$1`, source).Scan(&generationHash, &generationRaw); err != nil {
		test.Fatal(err)
	}
	if domain.HashContent(generationRaw) != generationHash {
		test.Fatal("generation fixture already lacks a valid hash proof")
	}
	tampered := []byte(`{}`)
	tag, err := storage.pool.Exec(ctx, `UPDATE doc_display_generations_v1 SET bytes=$2 WHERE source_hash=$1`, source, tampered)
	if err != nil || tag.RowsAffected() != 1 {
		test.Fatal("tamper generation bytes without updating their hash", tag.RowsAffected(), err)
	}
	var storedHash domain.ContentHash
	if err := storage.pool.QueryRow(ctx, `SELECT generation_hash,bytes FROM doc_display_generations_v1
 WHERE source_hash=$1`, source).Scan(&storedHash, &generationRaw); err != nil {
		test.Fatal(err)
	}
	if storedHash != generationHash || !bytes.Equal(generationRaw, tampered) || domain.HashContent(generationRaw) == storedHash {
		test.Fatal("tampering did not preserve the mismatched stored generation hash")
	}
	for count := 0; count < 100; count++ {
		step, err := storage.StepDocDisplayMaintenance(ctx)
		if errors.Is(err, domain.ErrIntegrity) {
			break
		}
		if err != nil {
			test.Fatal("unexpected maintenance failure for a generation hash mismatch", count, err)
		}
		if step.Deleted != 0 {
			test.Fatal("tampered generation allowed CAS deletion", step)
		}
		if count == 99 {
			test.Fatal("valid-JSON generation corruption did not fail closed")
		}
	}
	var phase string
	var objectCount int
	if err := storage.pool.QueryRow(ctx, `SELECT phase,
 (SELECT count(*) FROM doc_display_objects_v1 WHERE source_hash=$1)
 FROM doc_display_maintenance_v1 WHERE source_hash=$1`, source).Scan(&phase, &objectCount); err != nil {
		test.Fatal(err)
	}
	if phase != "roots_generation" || objectCount != len(objects) {
		test.Fatal("generation proof failure advanced cleanup or deleted CAS objects", phase, objectCount)
	}
	for _, object := range objects {
		raw, err := storage.loadDocDisplayObject(ctx, source, object.Hash)
		if err != nil || !bytes.Equal(raw, object.Bytes) {
			test.Fatal("generation proof failure changed CAS contents", object.Hash, err)
		}
	}
}
