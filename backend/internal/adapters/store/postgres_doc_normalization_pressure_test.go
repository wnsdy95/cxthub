//go:build postgres

package store

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestNormalizationCheckpointPGAutomaticObjectPressureRestartsAndPublishes(test *testing.T) {
	normalizationCheckpointPGAutomaticPressure(test, false)
}

func TestNormalizationCheckpointPGAutomaticBytePressureRestartsAndPublishes(test *testing.T) {
	normalizationCheckpointPGAutomaticPressure(test, true)
}

func normalizationCheckpointPGAutomaticPressure(test *testing.T, bytePressure bool) {
	test.Helper()
	storage, setup := chunkReusePG(test)
	rangeSpoolTempDir(test)
	fixture := legacyByteStagePGSource(test, storage, setup, 768<<10)
	work, job, trace := docNormalizationPGStart(test, storage, setup, fixture)
	defer work.Close()
	firstRootStarted := time.Now()
	normalizationCheckpointPGCut(test, work, setup, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
		return checkpoint.state.hash != "" && !checkpoint.busy() && !checkpoint.root.State.InputPending()
	})
	firstRootElapsed := time.Since(firstRootStarted)
	initial := work.checkpoint.state
	if work.source != nil || work.decoding != nil || work.normalization.canonical != nil {
		test.Fatal("acknowledged post-input root did not release input before pressure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	renew := func() {
		if err := storage.RenewDocRangeJob(ctx, job, time.Now().UTC(), time.Minute); err != nil {
			test.Fatal("pressure workload lost its real worker heartbeat", err)
		}
	}
	usage := func() (int64, int64) {
		var count, size int64
		if err := storage.pool.QueryRow(ctx, `SELECT checkpoint_objects,checkpoint_bytes FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1`, job.DocHash).Scan(&count, &size); err != nil {
			test.Fatal(err)
		}
		return count, size
	}
	renew()
	seedStarted := time.Now()
	before, beforeBytes := usage()
	const pressureObjects = 60000
	const pressureBytes = 2 << 30
	seedObjects := pressureObjects - before
	if bytePressure {
		seedObjects = (pressureBytes - beforeBytes + domain.LegacyCanonicalEncodingBytes - 1) / domain.LegacyCanonicalEncodingBytes
	}
	var seededBytes int64
	for ordinal := int64(0); ordinal < seedObjects; {
		objects := make([]legacyByteCheckpointObject, 0, domain.DocLegacyByteBackfillBatchBlocks)
		for len(objects) < domain.DocLegacyByteBackfillBatchBlocks && ordinal < seedObjects {
			object := legacyCheckpointPGObject(fmt.Sprintf("abandoned-pressure-%d", ordinal))
			if bytePressure {
				data := bytes.Repeat([]byte("p"), domain.LegacyCanonicalEncodingBytes)
				copy(data, object.data)
				object = legacyByteCheckpointObject{hash: domain.HashContent(data), data: data}
			}
			objects = append(objects, object)
			seededBytes += int64(len(object.data))
			ordinal++
		}
		if err := storage.stageLegacyNormalizationCheckpointObjects(ctx, job, initial, objects); err != nil {
			test.Fatal("valid bounded checkpoint writes could not reach pressure", ordinal, err)
		}
		if ordinal%1024 == 0 {
			renew()
		}
	}
	seedElapsed := time.Since(seedStarted)
	count, size := usage()
	if count != before+seedObjects || size != beforeBytes+seededBytes ||
		!bytePressure && count != pressureObjects || bytePressure && (size < pressureBytes || count >= pressureObjects) {
		test.Fatal("pressure fixture did not use the real independent counters", count, size)
	}
	bodyReads := trace.bodyReads.Load()
	queries := trace.queries.Load()
	pressureStarted := time.Now()
	pressureSeen, acknowledged := false, false
	peakHeap, peakSpool := int64(0), int64(0)
	steps := 0
	for ; steps < 4*pressureObjects/128+1024; steps++ {
		if steps%100 == 0 {
			renew()
		}
		previous, _ := usage()
		resources, err := work.RetainedResources()
		if err != nil || resources.HeapBytes < 0 || resources.SpoolBytes < 0 || resources.HeapBytes > 16<<20 {
			test.Fatal("pressure path lost retained resource accounting", resources, err)
		}
		peakHeap, peakSpool = max(peakHeap, resources.HeapBytes), max(peakSpool, resources.SpoolBytes)
		progress, err := work.Step(ctx, job)
		if err != nil || progress != outbound.DocRangeWorkPending || work.normalization == nil || work.normalization.canonical != nil {
			test.Fatal("automatic pressure bypassed its checkpoint barrier", steps, progress, err)
		}
		current, _ := usage()
		if removed := previous - current; removed > 128 {
			test.Fatal("pressure reclamation exceeded one bounded delete quantum", removed)
		}
		pressureSeen = pressureSeen || work.checkpoint.reclaim
		if pressureSeen && !work.checkpoint.busy() && work.checkpoint.state.sequence >= initial.sequence+2 {
			acknowledged = true
			break
		}
	}
	count, size = usage()
	if !pressureSeen || !acknowledged || count >= pressureObjects || size >= pressureBytes || trace.bodyReads.Load() != bodyReads {
		test.Fatal("automatic pressure failed to finish without rereading source", pressureSeen, acknowledged, count, size, trace.bodyReads.Load()-bodyReads)
	}
	pressureElapsed := time.Since(pressureStarted)
	test.Logf("byte_pressure=%t first_root=%s seed_objects=%d seed_bytes=%d seed_time=%s pressure_time=%s pressure_steps=%d worker_queries=%d retained_heap_peak=%d retained_spool_peak=%d remaining_objects=%d remaining_bytes=%d",
		bytePressure, firstRootElapsed, seedObjects, seededBytes, seedElapsed, pressureElapsed, steps+1, trace.queries.Load()-queries, peakHeap, peakSpool, count, size)
	normalizationCheckpointPGColdReclaim(test, storage, ctx, fixture, job, work)
	var retained int64
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1`, job.DocHash).Scan(&retained); err != nil || retained != 0 {
		test.Fatal("completed publication retained private checkpoint objects", retained, err)
	}
}
