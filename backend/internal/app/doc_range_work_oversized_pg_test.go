//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGDocRangeWorkKeepsPermitWhileEventExceedsParkingBudget(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	fixture.storage.ConfigureLegacyDerivedByteRangePublication(true)
	if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, fixture.hash); err != nil {
		test.Fatal(err)
	}
	document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{
		{Kind: domain.EventToolResult, Output: strings.Repeat("x", 9<<20)},
		historyMessage(domain.RoleUser, "oversized event boundary "+string(fixture.repo)),
	})
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	streamBytes, sourceSteps := docRangeWorkPGSourceQuanta(test, canonical)
	initialSteps := docRangeWorkPGInitialSteps(len(canonical), streamBytes)
	streamSteps := (streamBytes + (512 << 10) - 1) / (512 << 10)
	copyStart, copyComplete := initialSteps-2*streamSteps-1, initialSteps-streamSteps-2
	wantSteps := initialSteps + sourceSteps + 3
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
	foreground := make(chan error, 2)
	var readCompleted atomic.Int64
	var readFailure atomic.Pointer[error]
	waiting, previousOversized := false, false
	queuedReads, queuedCanonicalStep, queuedMetadataStep := int64(0), 0, 0
	canonicalOversizedSteps, metadataOversizedSteps := 0, 0
	resumedCanonicalStep, resumedMetadataStep := 0, 0
	var resumeBeforeQueue int64
	previousStepFinished := time.Now()
	probe.afterPrepare = func(bound context.Context, resources outbound.RootPublicationResources) error {
		if err := docRangeWorkPGResourcePhase(streamBytes, len(canonical), sourceSteps, 0, resources); err != nil {
			return err
		}
		return docRangeWorkPGNoByteStage(bound, fixture, document.Hash)
	}
	probe.afterStep = func(bound context.Context, step int, complete bool) error {
		stepElapsed := time.Since(previousStepFinished)
		previousStepFinished = time.Now()
		resources := probe.resources[step-1]
		if err := docRangeWorkPGResourcePhase(streamBytes, len(canonical), sourceSteps, step, resources); err != nil {
			return err
		}
		if complete != (step == wantSteps) {
			return fmt.Errorf("oversized metadata completed before its final chunk directory: step=%d source_steps=%d complete=%t", step, sourceSteps, complete)
		}
		if step == initialSteps+sourceSteps && resources.SpoolBytes <= int64(streamBytes) {
			return errors.New("oversized final source quantum did not flush its final event page")
		}
		if step > initialSteps+sourceSteps && resources.SpoolBytes <= probe.resources[step-2].SpoolBytes {
			return errors.New("oversized directory finalization did not flush its next metadata object")
		}
		if step <= initialSteps {
			if err := docRangeWorkPGNoByteStage(bound, fixture, document.Hash); err != nil {
				return err
			}
		}
		if ready, err := fixture.storage.HasDocByteRangeIndex(bound, fixture.repo, document.Hash); err != nil || ready {
			return fmt.Errorf("oversized private work advertised public byte readiness: ready=%t error=%v", ready, err)
		}
		if waiting {
			permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
			if permit == nil || !permit.live.Load() {
				return errors.New("oversized work lost its live bulk permit")
			}
			if previousOversized {
				if readCompleted.Load() != queuedReads-1 || permit.resumeWait.Load() != resumeBeforeQueue {
					var foregroundError error
					if failure := readFailure.Load(); failure != nil {
						foregroundError = *failure
					}
					return fmt.Errorf("foreground completed or permit resumed before bounded event release: step=%d previous=%+v current=%+v reads=%d queued=%d resume_before=%d resume_after=%d probe_interval=%s read_error=%v", step, probe.resources[step-2], resources, readCompleted.Load(), queuedReads, resumeBeforeQueue, permit.resumeWait.Load(), stepElapsed, foregroundError)
				}
			} else {
				if readCompleted.Load() != queuedReads || permit.resumeWait.Load() <= resumeBeforeQueue {
					return errors.New("bounded work advanced without foreground progress and owned-permit resumption")
				}
				if resources.HeapBytes > maxParkedRootHeapBytes {
					return errors.New("foreground completion did not occur at a bounded scanner boundary")
				}
				if queuedReads == 1 {
					resumedCanonicalStep = step
				} else {
					resumedMetadataStep = step
				}
				waiting = false
			}
		}
		previousOversized = resources.HeapBytes > maxParkedRootHeapBytes
		if !previousOversized {
			return nil
		}
		permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
		owner, _ := bound.Value(resourceYieldOwnerKey{}).(*resourceYieldOwner)
		if permit == nil || owner == nil || owner.permit != permit || !permit.live.Load() {
			return errors.New("oversized work does not own its live bulk permit")
		}
		switch {
		case step >= copyStart && step < copyComplete:
			canonicalOversizedSteps++
			if queuedCanonicalStep != 0 {
				return nil
			}
			queuedCanonicalStep = step
		case step > initialSteps && step < initialSteps+sourceSteps:
			metadataOversizedSteps++
			if queuedMetadataStep != 0 {
				return nil
			}
			queuedMetadataStep = step
		default:
			return fmt.Errorf("oversized heap escaped canonical or metadata partial-event phases: step=%d resources=%+v", step, resources)
		}
		if waiting {
			return errors.New("metadata oversized phase began before the canonical-scanner foreground read resumed")
		}
		queuedReads++
		waiting, resumeBeforeQueue = true, permit.resumeWait.Load()
		go func() {
			page, err := fixture.service.ReadDocEvents(ctx, fixture.repo, fixture.hash, "", 0, 20)
			if err == nil && (len(page.Events) != 20 || page.Hash != fixture.hash || page.Next != 20) {
				err = errors.New("queued foreground read lost its pinned page")
			}
			if err != nil {
				readFailure.Store(&err)
			}
			readCompleted.Add(1)
			foreground <- err
		}()
		waitResourceQueue(test, gate, ResourceBulk, 1)
		return nil
	}
	if err := fixture.service.ProcessDocRangeJobs(ctx, 1); err != nil {
		test.Fatal(err)
	}
	if queuedReads != 2 || waiting || canonicalOversizedSteps < 2 || metadataOversizedSteps < 2 || probe.steps != wantSteps ||
		probe.published != 1 || probe.closed != 1 || len(probe.initial) != 1 {
		test.Fatal("actual giant-event work did not cross the unchanged parking budget and finish", canonicalOversizedSteps, metadataOversizedSteps, probe)
	}
	if queuedCanonicalStep < copyStart || queuedCanonicalStep >= copyComplete ||
		resumedCanonicalStep <= copyComplete || resumedCanonicalStep > initialSteps ||
		queuedMetadataStep <= initialSteps || queuedMetadataStep >= initialSteps+sourceSteps ||
		resumedMetadataStep <= initialSteps+sourceSteps || resumedMetadataStep > wantSteps {
		test.Fatal("foreground probes did not cover oversized partial events and bounded release in both phases", queuedCanonicalStep, resumedCanonicalStep, queuedMetadataStep, resumedMetadataStep)
	}
	for range 2 {
		select {
		case err := <-foreground:
			if err != nil {
				test.Fatal(err)
			}
		case <-ctx.Done():
			test.Fatal("queued foreground read failed to resume after canonical or metadata scanner release", ctx.Err())
		}
	}
	assertResourcesReleased(test, gate)
	assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
	generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
	if generation.Version != 5 || generation.Hash != document.Hash || generation.ChunkCount() != sourceSteps {
		test.Fatal("oversized retained work changed the published byte generation", generation)
	}
	test.Logf("steps=%d canonical_oversized_quanta=%d metadata_oversized_quanta=%d last_resources=%+v", probe.steps, canonicalOversizedSteps, metadataOversizedSteps, probe.resources[len(probe.resources)-1])
}
