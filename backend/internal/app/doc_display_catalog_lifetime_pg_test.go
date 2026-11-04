//go:build postgres

package app

import (
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func assertDocDisplayCatalogRetiredPG(test *testing.T, fixture docViewPGFixture, source domain.ContentHash) {
	test.Helper()
	for table, image := range docDisplayCatalogImagePG(test, fixture) {
		if image.Rows != 0 {
			test.Fatal("terminal completion recreated or retained private catalog rows", table, image.Rows)
		}
	}
	var epoch, generation string
	var retired bool
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT lifecycle.catalog_epoch::text,admission.generation::text,
 lifecycle.state='retired' AND lifecycle.retire_phase='empty' AND lifecycle.retire_scope=''
 AND lifecycle.retire_generation=admission.generation FROM doc_display_catalog_lifecycle_v1 lifecycle
 JOIN doc_display_sources_v1 source USING(source_hash)
 JOIN doc_display_catalog_admissions_v1 admission ON admission.event_hash=source.event_hash AND admission.profile=source.profile
 AND admission.source_hash=source.source_hash WHERE lifecycle.source_hash=$1`, source).Scan(&epoch, &generation, &retired); err != nil || !retired || domain.ValidateDocDisplayCatalogEpoch(epoch) != nil || domain.ValidateDocDisplayCatalogEpoch(generation) != nil {
		test.Fatal("private cleanup did not preserve its durable retired epoch and Source incarnation", retired, epoch, generation, err)
	}
}

func TestPGDocDisplayCompletedEvidenceSurvivesPrivateCatalogRemoval(test *testing.T) {
	fixture := newPublishedReuseFixturePG(test)
	peer := finishPublishedReusePeerPG(test, fixture)
	if complete, err := fixture.right.service.buildDocEventDisplay(fixture.right.ctx, fixture.rightJob); err != nil || !complete {
		test.Fatal("fixture must not erase catalogs with unfinished receivers", complete, err)
	}
	expectedGeneration, expectedPages := readDocDisplayCatalogPagesPG(test, fixture.left, fixture.leftJob.Target)
	owner, err := peer.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	before := docDisplayCatalogImagePG(test, fixture.left)
	for table, image := range before {
		if image.Rows == 0 {
			test.Fatal("lifetime fixture did not exercise all private catalogs", table)
		}
	}
	retiredObjects, retiredBytes := drainDocDisplayMaintenancePG(test, fixture.left)
	assertRemoved := func() {
		test.Helper()
		assertDocDisplayCatalogRetiredPG(test, fixture.left, owner)
	}
	assertRemoved()
	if _, err := fixture.left.pool.Exec(fixture.left.ctx, `DELETE FROM doc_display_generations_v1 WHERE event_hash=$1 AND profile=$2`, fixture.leftJob.Target.Event.Hash, fixture.leftJob.Target.DisplayProfile()); err != nil {
		test.Fatal(err)
	}
	deleted, deletedBytes := drainDocDisplayMaintenancePG(test, fixture.left)
	deleted += retiredObjects
	deletedBytes += retiredBytes
	queued, err := fixture.left.storage.EnqueueDocDisplay(fixture.left.ctx, fixture.leftJob.Target, time.Now().UTC())
	if err != nil || queued.State != "waiting" {
		test.Fatal("missing serving descriptor did not queue its checked repair", queued, err)
	}
	probe := &docDisplayCatalogReuseProbePG{PostgresStore: fixture.left.storage}
	restarted := NewService(fixture.left.storage, fixture.left.storage, nil, nil, fixture.left.storage)
	restarted.ConfigureDocumentDisplay(probe)
	if err := restarted.ProcessDocDisplayJobs(fixture.left.ctx, 1); err != nil || probe.saveCalls != 0 || probe.completeCalls != 1 {
		test.Fatal("terminal repair depended on removed private SQL catalogs", probe, err)
	}
	actualGeneration, actualPages := readDocDisplayCatalogPagesPG(test, fixture.left, fixture.leftJob.Target)
	if !reflect.DeepEqual(expectedGeneration, actualGeneration) || !reflect.DeepEqual(expectedPages, actualPages) {
		test.Fatal("terminal CAS-only repair changed generation/text/original-byte evidence")
	}
	assertRemoved()
	third := newDocDisplayPGFixture(test)
	thirdJob := claimPublishedReusePG(test, third, fixture.event)
	reused := &docDisplayCatalogReuseProbePG{PostgresStore: third.storage}
	third.service.ConfigureDocumentDisplay(reused)
	if complete, err := third.service.buildDocEventDisplay(third.ctx, thirdJob); err != nil || !complete || reused.saveCalls != 0 || reused.completeCalls != 1 {
		test.Fatal("new same-event receiver depended on retired private catalog rows", complete, reused, err)
	}
	_, thirdPages := readDocDisplayCatalogPagesPG(test, third, thirdJob.Target)
	if !reflect.DeepEqual(thirdPages, expectedPages) {
		test.Fatal("new receiver changed text/source mappings after private catalog removal")
	}
	assertRemoved()
	test.Logf("private_catalog_tables=7 remaining_private_rows=0 actual_gc_deleted_objects=%d actual_gc_deleted_payload_bytes=%d repair_batches=1 repair_preparation_saves=0 new_receiver_batches=1 new_receiver_preparation_saves=0", deleted, deletedBytes)
	test.Log("Actual automatic retirement removes private catalogs in bounded committed steps; published CAS-only repair and a new receiver retain exact text and original-byte evidence. Store-level audit separately verifies nested writes, endpoint FKs, durable restart and no collector job locks.")
}
