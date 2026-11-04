//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type displayReuseFootprintPG struct {
	AccountingScope        string `json:"accounting_scope"`
	Rows                   int64  `json:"rows"`
	StoredTupleBytes       int64  `json:"stored_tuple_bytes"`
	LogicalPayloadBytes    int64  `json:"logical_payload_bytes"`
	AllocatedRelationBytes int64  `json:"allocated_relation_bytes"`
	AllocatedIndexBytes    int64  `json:"allocated_index_bytes"`
}

func displayReuseFootprintSnapshotPG(test *testing.T, fixture docViewPGFixture) map[string]displayReuseFootprintPG {
	test.Helper()
	var result map[string]displayReuseFootprintPG
	if err := json.Unmarshal([]byte(docDisplayStorageFootprintPG(test, fixture)), &result); err != nil || len(result) != 18 {
		test.Fatal("incomplete footprint of seventeen isolated-fixture table totals and one global singleton table", len(result), err)
	}
	cursor, exists := result["doc_display_maintenance_cursor_v1"]
	if !exists || cursor.AccountingScope != "global_singleton" || cursor.Rows != 1 || cursor.StoredTupleBytes <= 0 || cursor.LogicalPayloadBytes < 0 || cursor.AllocatedRelationBytes <= 0 || cursor.AllocatedIndexBytes <= 0 {
		test.Fatal("global maintenance cursor lacks singleton storage accounting", cursor)
	}
	return result
}

func displayReuseAdmissionPG(test *testing.T, fixture docViewPGFixture, target domain.DocDisplayTarget, receipts int) string {
	test.Helper()
	var generation string
	var revision int64
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT admission.generation::text,admission.admission_revision
 FROM doc_display_catalog_admissions_v1 admission JOIN doc_display_sources_v1 source
 ON source.event_hash=admission.event_hash AND source.profile=admission.profile AND source.source_hash=admission.source_hash
 WHERE admission.event_hash=$1 AND admission.profile=$2`, target.Event.Hash, target.DisplayProfile()).Scan(&generation, &revision); err != nil || generation == "" || revision != int64(receipts)*3 {
		test.Fatal("receipt admission/source binding or lifecycle membership revision changed", receipts, generation, revision, err)
	}
	return generation
}

type displayReuseSQLCostPG struct {
	Statements, Rows, SharedHits, SharedReads, TempWrites int64
	ExecutionMS, WALBytes                                 float64
}

func resetDisplayReuseSQLCostPG(test *testing.T, fixture docViewPGFixture, enabled bool) {
	test.Helper()
	if enabled {
		if _, err := fixture.pool.Exec(fixture.ctx, `SELECT pg_stat_statements_reset(0,(SELECT oid FROM pg_database WHERE datname=current_database()),0)`); err != nil {
			test.Fatal("requested SQL measurement requires preloaded pg_stat_statements", err)
		}
	}
}

func readDisplayReuseSQLCostPG(test *testing.T, fixture docViewPGFixture, enabled bool) displayReuseSQLCostPG {
	test.Helper()
	var result displayReuseSQLCostPG
	if !enabled {
		return result
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT coalesce(sum(calls),0),coalesce(sum(rows),0),
 coalesce(sum(shared_blks_hit),0),coalesce(sum(shared_blks_read),0),coalesce(sum(temp_blks_written),0),
 coalesce(sum(total_exec_time),0),coalesce(sum(wal_bytes),0)
 FROM pg_stat_statements WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database()) AND toplevel
 AND query NOT LIKE '%pg_stat_statements%'
 AND query NOT LIKE 'SELECT state,reason FROM doc_display_jobs_v1%'`).Scan(
		&result.Statements, &result.Rows, &result.SharedHits, &result.SharedReads, &result.TempWrites, &result.ExecutionMS, &result.WALBytes); err != nil {
		test.Fatal("SQL measurement", err)
	}
	return result
}

func logDisplayReuseSQLHotspotsPG(test *testing.T, fixture docViewPGFixture, phase string, enabled bool) {
	test.Helper()
	if !enabled {
		return
	}
	rows, err := fixture.pool.Query(fixture.ctx, `SELECT calls,total_exec_time,rows,query FROM pg_stat_statements
 WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database()) AND toplevel
 AND query NOT LIKE '%pg_stat_statements%' AND query NOT LIKE 'SELECT state,reason FROM doc_display_jobs_v1%'
 ORDER BY calls DESC,queryid LIMIT 10`)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var hotspot struct {
			Statements, Rows int64
			ExecutionMS      float64
			SQL              string
		}
		if err := rows.Scan(&hotspot.Statements, &hotspot.ExecutionMS, &hotspot.Rows, &hotspot.SQL); err != nil {
			test.Fatal(err)
		}
		raw, err := json.Marshal(hotspot)
		if err != nil {
			test.Fatal(err)
		}
		test.Logf("phase=%s sql_hotspot=%s", phase, raw)
	}
	if err := rows.Err(); err != nil {
		test.Fatal(err)
	}
}

func TestPGDocDisplayRepeatedSourceCostAndRetention(test *testing.T) {
	test.Setenv("CXT_TEST_DSN", a11IsolatedPGDSN(test, context.Background()))
	fixture := newDocDisplayPGFixture(test)
	measureSQL := os.Getenv("CXT_DOC_DISPLAY_COST") == "1"
	if measureSQL {
		if _, err := fixture.pool.Exec(fixture.ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err != nil {
			test.Fatal(err)
		}
	}
	rows := make([]any, 32)
	for index := range rows {
		rows[index] = map[string]any{"ordinal": index, "text": fmt.Sprintf("cost-row-%02d🙂", index), "padding": strings.Repeat("p", 5000), "nested": []any{true, nil, "shared"}}
	}
	event := domain.CIREvent{Kind: domain.EventToolCall, CallID: "repeated-source-cost", ToolName: "generic", Input: map[string]any{"rows": rows}}
	queue := func(ordinal int) domain.DocDisplayTarget {
		measured := ordinal == 0 || ordinal == 1 || ordinal == 10 || ordinal == 100
		var setupBefore, setupAfter runtime.MemStats
		var setupStarted time.Time
		if measured {
			resetDisplayReuseSQLCostPG(test, fixture, measureSQL)
			runtime.ReadMemStats(&setupBefore)
			setupStarted = time.Now()
		}
		cir := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}}
		cir.Envelope.SessionOriginID = fmt.Sprintf("cost-receipt-%d", ordinal)
		hash := seedDocViewBoundaryPG(test, fixture, cir)
		target := docDisplayTargetPG(test, fixture, hash, 0)
		target.Profile = domain.DocDisplayProjectionProfile
		if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
			test.Fatal(err)
		}
		if measured {
			elapsed := time.Since(setupStarted)
			runtime.ReadMemStats(&setupAfter)
			cost := readDisplayReuseSQLCostPG(test, fixture, measureSQL)
			test.Logf("phase=setup receipt=%d setup_elapsed_ms=%.3f setup_total_alloc_bytes=%d heap_before_bytes=%d heap_after_bytes=%d sql_measured=%t top_level_statements=%d sql=%+v", ordinal, float64(elapsed.Microseconds())/1000, setupAfter.TotalAlloc-setupBefore.TotalAlloc, setupBefore.HeapAlloc, setupAfter.HeapAlloc, measureSQL, cost.Statements, cost)
			if ordinal == 1 {
				logDisplayReuseSQLHotspotsPG(test, fixture, "setup", measureSQL)
			}
		}
		return target
	}
	first := queue(0)
	resetDisplayReuseSQLCostPG(test, fixture, measureSQL)
	var memoryBefore, memoryAfter runtime.MemStats
	runtime.ReadMemStats(&memoryBefore)
	started := time.Now()
	firstBatches := finishDocDisplayCatalogJobPG(test, fixture, first)
	firstElapsed := time.Since(started)
	runtime.ReadMemStats(&memoryAfter)
	firstCost := readDisplayReuseSQLCostPG(test, fixture, measureSQL)
	test.Logf("phase=cold batches=%d worker_elapsed_ms=%.3f worker_total_alloc_bytes=%d heap_before_bytes=%d heap_after_bytes=%d sql_measured=%t top_level_statements=%d sql=%+v", firstBatches, float64(firstElapsed.Microseconds())/1000, memoryAfter.TotalAlloc-memoryBefore.TotalAlloc, memoryBefore.HeapAlloc, memoryAfter.HeapAlloc, measureSQL, firstCost.Statements, firstCost)
	logDisplayReuseSQLHotspotsPG(test, fixture, "cold", measureSQL)
	checkpoint := readDocDisplayCatalogReceiptPG(test, fixture, first).Checkpoint
	admissionGeneration := displayReuseAdmissionPG(test, fixture, first, 1)
	baseline := displayReuseFootprintSnapshotPG(test, fixture)
	catalogs := docDisplayCatalogImagePG(test, fixture)
	_, expectedPages := readDocDisplayCatalogPagesPG(test, fixture, first)
	var firstReuseCost displayReuseSQLCostPG
	var last domain.DocDisplayTarget
	for ordinal := 1; ordinal <= 100; ordinal++ {
		last = queue(ordinal)
		if last.Event.Hash != first.Event.Hash || last.DocHash == first.DocHash || last.Binding == first.Binding {
			test.Fatal("cost fixture must share only exact event work, not document identity")
		}
		measured := ordinal == 1 || ordinal == 10 || ordinal == 100
		if measured {
			resetDisplayReuseSQLCostPG(test, fixture, measureSQL)
			runtime.ReadMemStats(&memoryBefore)
			started = time.Now()
		}
		probe := &docDisplayCatalogReuseProbePG{PostgresStore: fixture.storage}
		restarted := NewService(fixture.storage, fixture.storage, nil, nil, fixture.storage)
		restarted.ConfigureDocumentDisplay(probe)
		if err := restarted.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil || probe.saveCalls != 0 || probe.completeCalls != 1 {
			test.Fatal("repeated source reconstructed or skipped ordinary completion", ordinal, probe, err)
		}
		if measured {
			elapsed := time.Since(started)
			runtime.ReadMemStats(&memoryAfter)
			cost := readDisplayReuseSQLCostPG(test, fixture, measureSQL)
			if ordinal == 1 {
				firstReuseCost = cost
			}
			if measureSQL && (cost.Statements == 0 || cost.Statements > firstReuseCost.Statements+2 || cost.TempWrites != 0) {
				test.Fatal("receiving-job statement work grew with earlier receipts or spilled", ordinal, firstReuseCost, cost)
			}
			test.Logf("phase=reuse receipt=%d batches=1 preparation_saves=0 normal_completions=1 worker_elapsed_ms=%.3f worker_total_alloc_bytes=%d heap_before_bytes=%d heap_after_bytes=%d sql_measured=%t top_level_statements=%d sql=%+v", ordinal, float64(elapsed.Microseconds())/1000, memoryAfter.TotalAlloc-memoryBefore.TotalAlloc, memoryBefore.HeapAlloc, memoryAfter.HeapAlloc, measureSQL, cost.Statements, cost)
			if ordinal == 1 {
				logDisplayReuseSQLHotspotsPG(test, fixture, "reuse", measureSQL)
			}
		}
		receipt := readDocDisplayCatalogReceiptPG(test, fixture, last)
		if receipt.State != "completed" || receipt.Checkpoint != checkpoint {
			test.Fatal("receipt lost exact completed work", ordinal, receipt.State, receipt.Checkpoint)
		}
		if measured {
			current := displayReuseFootprintSnapshotPG(test, fixture)
			for _, table := range []string{"doc_display_sources_v1", "doc_display_objects_v1", "doc_display_generations_v1", "doc_display_object_edges_v1", "doc_display_catalog_admissions_v1", "doc_display_catalog_lifecycle_v1"} {
				before, after := baseline[table], current[table]
				if after.Rows != before.Rows || after.LogicalPayloadBytes != before.LogicalPayloadBytes {
					test.Fatal("same-source receipt multiplied retained shared data", ordinal, table, before, after)
				}
			}
			if displayReuseAdmissionPG(test, fixture, last, ordinal+1) != admissionGeneration {
				test.Fatal("same-source receipt changed its admission generation", ordinal)
			}
			if current["doc_display_jobs_v1"].Rows != int64(ordinal+1) {
				test.Fatal("reuse fabricated or omitted durable own receipts", ordinal, current["doc_display_jobs_v1"])
			}
			assertDocDisplayCatalogImagePG(test, fixture, catalogs)
			test.Logf("receipts=%d footprint=%s", ordinal+1, docDisplayStorageFootprintPG(test, fixture))
		}
	}
	deleted, deletedBytes := drainDocDisplayMaintenancePG(test, fixture)
	receipt := readDocDisplayCatalogReceiptPG(test, fixture, last)
	assertDocDisplayCatalogRetiredPG(test, fixture, receipt.Source)
	_, actualPages := readDocDisplayCatalogPagesPG(test, fixture, last)
	if !reflect.DeepEqual(actualPages, expectedPages) {
		test.Fatal("shared completed text/source mapping changed after 100 receipts and cleanup")
	}
	test.Logf("receipts=101 after_cleanup_deleted_objects=%d after_cleanup_deleted_payload_bytes=%d footprint=%s", deleted, deletedBytes, docDisplayStorageFootprintPG(test, fixture))
	var retiredCensus bool
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT EXISTS(SELECT 1 FROM doc_display_catalog_census_v1 census
 JOIN doc_display_catalog_lifecycle_v1 lifecycle USING(source_hash)
 JOIN doc_display_sources_v1 source USING(source_hash)
 JOIN doc_display_catalog_admissions_v1 admission ON admission.event_hash=source.event_hash AND admission.profile=source.profile
 WHERE lifecycle.state='retired' AND census.phase='clear' AND census.catalog_epoch=lifecycle.catalog_epoch
 AND census.generation=lifecycle.retire_generation AND lifecycle.retire_generation=admission.generation
 AND census.admission_revision=admission.admission_revision AND census.source_revision<=source.reference_revision
 AND admission.source_hash=source.source_hash)`).Scan(&retiredCensus); err != nil || !retiredCensus {
		test.Fatal("completed retirement lost its epoch-qualified census or Source incarnation", retiredCensus, err)
	}
	test.Log("Worker counters exclude document upload/index setup and test observation SQL; setup is reported separately. Top-level PostgreSQL statements are not network round trips, nested trigger work, selected byte counts or production p95. TotalAlloc is period allocation, not peak RSS. Logical tuples, payloads and physical allocation remain separate. The maintenance cursor is one global singleton, counted once rather than per source. Relation/index allocation is database-wide; relation totals already include indexes.")
}
