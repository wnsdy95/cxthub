//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func docRangeWorkPGSourceQuanta(test testing.TB, canonical []byte) (int, int) {
	test.Helper()
	var parts struct {
		Envelope json.RawMessage   `json:"envelope"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(canonical, &parts); err != nil {
		test.Fatal(err)
	}
	var envelope domain.CIREnvelope
	if err := json.Unmarshal(parts.Envelope, &envelope); err != nil || len(parts.Envelope) > 4<<10 || len(envelope.SourceModels) > 16 {
		test.Fatal("fixed phase fixture no longer has a small single-quantum native/encoder header", err, len(parts.Envelope), len(envelope.SourceModels))
	}
	streamBytes := max(len(parts.Events)-1, 0)
	for _, event := range parts.Events {
		streamBytes += len(event)
	}
	return streamBytes, max((streamBytes+domain.ConversationManifestChunkBytes-1)/domain.ConversationManifestChunkBytes, 1)
}

const docRangeWorkPGSmallHeaderSteps = 8

func docRangeWorkPGInitialSteps(rawBytes, streamBytes int) int {
	storedSteps := (rawBytes + (512 << 10) - 1) / (512 << 10)
	streamSteps := (streamBytes + (512 << 10) - 1) / (512 << 10)
	return 2*storedSteps + rawBytes/(512<<10) + 2*streamSteps + 4 + docRangeWorkPGSmallHeaderSteps
}

func docRangeWorkPGResourcePhase(streamBytes, rawBytes, sourceQuanta, step int, resources outbound.RootPublicationResources) error {
	if resources.KeepPermit || resources.HeapBytes <= 0 || resources.SpoolBytes < 0 || resources.SpoolBytes > maxParkedRootSpoolBytes {
		return fmt.Errorf("invalid retained work resources at step %d: %+v", step, resources)
	}
	storedSteps := (rawBytes + (512 << 10) - 1) / (512 << 10)
	decodedSteps := storedSteps + rawBytes/(512<<10) + 1
	decisionStep := decodedSteps + storedSteps + 1 + docRangeWorkPGSmallHeaderSteps
	streamSteps := (streamBytes + (512 << 10) - 1) / (512 << 10)
	copyComplete := decisionStep + streamSteps
	initialSteps := docRangeWorkPGInitialSteps(rawBytes, streamBytes)
	if step < 0 || step > initialSteps+sourceQuanta+3 {
		return fmt.Errorf("work advanced outside canonical proof, metadata and directory phases: step=%d", step)
	}
	if step <= storedSteps {
		wantSpool := min(int64(step)*(512<<10), int64(rawBytes))
		if resources.SpoolBytes != wantSpool || resources.HeapBytes > 1<<20 {
			return fmt.Errorf("provisional stored capture retained more than its bounded spans at step %d: resources=%+v want_spool=%d", step, resources, wantSpool)
		}
		return nil
	}
	if step <= decodedSteps {
		wantSpool := int64(rawBytes) + min(int64(step-storedSteps)*(512<<10), int64(rawBytes))
		if resources.SpoolBytes != wantSpool || resources.HeapBytes >= 2<<20 {
			return fmt.Errorf("private plaintext decode retained more than its bounded spans at step %d: resources=%+v want_spool=%d", step, resources, wantSpool)
		}
		return nil
	}
	if step <= decisionStep {
		if resources.SpoolBytes != int64(rawBytes) || resources.HeapBytes >= 2<<20 {
			return fmt.Errorf("stored-source release, input hash or canonical decision retained whole-body proof at step %d: %+v", step, resources)
		}
		return nil
	}
	if step <= copyComplete {
		copiedBytes := min((step-decisionStep)*(512<<10), streamBytes)
		wantSpool := int64(rawBytes) + int64(copiedBytes)
		if resources.SpoolBytes != wantSpool {
			return fmt.Errorf("canonical stream copy prefix at step %d: resources=%+v want_spool=%d", step, resources, wantSpool)
		}
		if step < copyComplete {
			if resources.HeapBytes < 2*int64(copiedBytes) || resources.HeapBytes >= (2<<20)+4*int64(copiedBytes) {
				return fmt.Errorf("canonical scanner partial-event capacity missing or unbounded at step %d: resources=%+v copied=%d", step, resources, copiedBytes)
			}
		} else if resources.HeapBytes >= 2<<20 {
			return fmt.Errorf("completed canonical copy retained its scanner event at step %d: %+v", step, resources)
		}
		return nil
	}
	if step < initialSteps {
		if resources.SpoolBytes != int64(rawBytes)+int64(streamBytes) || resources.HeapBytes >= 2<<20 {
			return fmt.Errorf("canonical output verification or seal retained a whole-body proof or scanner event at step %d: %+v", step, resources)
		}
		return nil
	}
	if step == initialSteps {
		if resources.SpoolBytes != int64(streamBytes) || resources.HeapBytes >= 2<<20 {
			return fmt.Errorf("canonical handoff did not release decoded source and raw plan before source-backed metadata: %+v", resources)
		}
	} else if resources.SpoolBytes < int64(streamBytes) {
		return fmt.Errorf("metadata work lost its complete source spool at step %d: %+v", step, resources)
	}
	return nil
}

func docRangeWorkPGNoByteStage(ctx context.Context, fixture docViewPGFixture, hash domain.ContentHash) error {
	var stages, parts, objects int
	err := fixture.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1),
 (SELECT count(*) FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$1),
 (SELECT count(*) FROM doc_legacy_byte_stage_objects_v1 WHERE doc_hash=$1)`, hash).Scan(&stages, &parts, &objects)
	if err != nil {
		return err
	}
	if stages != 0 || parts != 0 || objects != 0 || fixture.storage.InTransaction(ctx) {
		return fmt.Errorf("canonical source proof touched SQL byte staging or retained a transaction: stages=%d parts=%d objects=%d", stages, parts, objects)
	}
	return nil
}

func TestPGDocRangeWorkYieldsToQueuedForegroundBetweenLocalSpoolSteps(test *testing.T) {
	for _, phase := range []string{"stored-capture", "decoded-source", "canonical-copy", "canonical-copy-verify-boundary"} {
		test.Run(phase, func(test *testing.T) {
			testPGDocRangeWorkYieldsToQueuedForeground(test, phase)
		})
	}
}

func testPGDocRangeWorkYieldsToQueuedForeground(test *testing.T, phase string) {
	fixture := newDocViewPGFixture(test)
	fixture.storage.ConfigureLegacyDerivedByteRangePublication(true)
	if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, fixture.hash); err != nil {
		test.Fatal(err)
	}
	document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{
		{Kind: domain.EventToolResult, Output: strings.Repeat("x", 1<<20)},
		historyMessage(domain.RoleUser, "canonical source boundary "+string(fixture.repo)),
	})
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	streamBytes, sourceQuanta := docRangeWorkPGSourceQuanta(test, canonical)
	if sourceQuanta != 3 {
		test.Fatal("fixture did not require three canonical stream blocks", streamBytes, sourceQuanta)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, document.Hash, canonical); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	probe := &docRangeWorkPGProbe{PostgresStore: fixture.storage}
	fixture.service.blobs = probe
	gate := &resourceAdmission{wait: resourceWaitLimit}
	ctx, cancel := context.WithTimeout(context.WithValue(fixture.ctx, resourceAdmissionKey{}, gate), 30*time.Second)
	defer cancel()
	foreground := make(chan error, 1)
	var readCompleted atomic.Bool
	probe.afterPrepare = func(bound context.Context, resources outbound.RootPublicationResources) error {
		if err := docRangeWorkPGResourcePhase(streamBytes, len(canonical), sourceQuanta, 0, resources); err != nil {
			return err
		}
		return docRangeWorkPGNoByteStage(bound, fixture, document.Hash)
	}
	initialSteps := docRangeWorkPGInitialSteps(len(canonical), streamBytes)
	streamSteps := (streamBytes + (512 << 10) - 1) / (512 << 10)
	copyComplete := initialSteps - streamSteps - 2
	queueStep := copyComplete - streamSteps + 1
	switch phase {
	case "stored-capture":
		queueStep = 1
	case "decoded-source":
		queueStep = (len(canonical)+(512<<10)-1)/(512<<10) + 1
	case "canonical-copy-verify-boundary":
		queueStep = copyComplete
	}
	wantSteps := initialSteps + sourceQuanta + 3
	probe.afterStep = func(bound context.Context, step int, complete bool) error {
		resources := probe.resources[step-1]
		if err := docRangeWorkPGResourcePhase(streamBytes, len(canonical), sourceQuanta, step, resources); err != nil {
			return err
		}
		if complete != (step == wantSteps) {
			return fmt.Errorf("canonical-source worker completed outside its final directory: step=%d complete=%t", step, complete)
		}
		if step <= initialSteps {
			if err := docRangeWorkPGNoByteStage(bound, fixture, document.Hash); err != nil {
				return err
			}
		}
		if ready, err := fixture.storage.HasDocByteRangeIndex(bound, fixture.repo, document.Hash); err != nil || ready {
			return fmt.Errorf("unpublished canonical-source work advertised byte readiness: ready=%t error=%v", ready, err)
		}
		if step < queueStep {
			return nil
		}
		if step > queueStep {
			permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
			if !readCompleted.Load() || permit == nil || !permit.live.Load() || permit.resumeWait.Load() <= 0 {
				return errors.New("next canonical source step ran before queued foreground completion and owned-permit resumption")
			}
			return nil
		}
		if resources.HeapBytes > maxParkedRootHeapBytes {
			return errors.New("canonical-source foreground fixture cannot park its actual scanner event")
		}
		go func() {
			page, err := fixture.service.ReadDocEvents(ctx, fixture.repo, fixture.hash, "", 0, 20)
			if err == nil && (len(page.Events) != 20 || page.Events[0].Seq != 0 || page.Next != 20 || page.Hash != fixture.hash) {
				err = errors.New("foreground page lost its pinned range between canonical source steps")
			}
			readCompleted.Store(true)
			foreground <- err
		}()
		waitResourceQueue(test, gate, ResourceBulk, 1)
		return nil
	}
	if err := fixture.service.ProcessDocRangeJobs(ctx, 1); err != nil || probe.steps != wantSteps || probe.published != 1 || probe.closed != 1 || len(probe.initial) != 1 {
		test.Fatal("canonical-source work did not yield, resume and publish exactly once", err, probe)
	}
	select {
	case err := <-foreground:
		if err != nil {
			test.Fatal(err)
		}
	case <-ctx.Done():
		test.Fatal("foreground read did not complete between canonical source steps", ctx.Err())
	}
	assertResourcesReleased(test, gate)
	assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
	generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
	if generation.Version != 5 || generation.Hash != document.Hash || generation.ChunkCount() != sourceQuanta {
		test.Fatal("canonical-source foreground interleaving changed the published generation", generation)
	}
}
