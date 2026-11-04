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

	"github.com/klauspost/compress/zstd"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGDocRangeWorkCompressedDecodeKeepsPermitUntilDecoderRelease(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	fixture.storage.ConfigureLegacyDerivedByteRangePublication(true)
	if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, fixture.hash); err != nil {
		test.Fatal(err)
	}
	document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{
		{Kind: domain.EventToolResult, Output: strings.Repeat("x", 1<<20)},
		historyMessage(domain.RoleUser, "opaque decoder boundary "+string(fixture.repo)),
	})
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		test.Fatal(err)
	}
	stored := encoder.EncodeAll(canonical, nil)
	encoder.Close()
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, document.Hash, stored); err != nil {
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
	pinnedSteps, firstPinned, released := 0, 0, 0
	var resumeBefore int64
	probe.afterStep = func(bound context.Context, step int, complete bool) error {
		resources := probe.resources[step-1]
		permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
		if permit == nil || !permit.live.Load() {
			return errors.New("compressed work lost its live owned permit")
		}
		if resources.KeepPermit {
			if resources.HeapBytes <= 0 || resources.HeapBytes >= maxParkedRootHeapBytes || resources.SpoolBytes > maxParkedRootSpoolBytes || complete || released != 0 {
				return fmt.Errorf("decoder retention fixture did not isolate its explicit permit veto: %+v", resources)
			}
			pinnedSteps++
			if firstPinned != 0 {
				if readCompleted.Load() || permit.resumeWait.Load() != resumeBefore {
					return errors.New("live opaque decoder parked between quanta")
				}
				return nil
			}
			firstPinned, resumeBefore = step, permit.resumeWait.Load()
			go func() {
				page, err := fixture.service.ReadDocEvents(ctx, fixture.repo, fixture.hash, "", 0, 20)
				if err == nil && (len(page.Events) != 20 || page.Events[0].Seq != 0 || page.Next != 20 || page.Hash != fixture.hash) {
					err = errors.New("queued decoder foreground lost its pinned page")
				}
				readCompleted.Store(true)
				foreground <- err
			}()
			waitResourceQueue(test, gate, ResourceBulk, 1)
			return nil
		}
		if firstPinned == 0 {
			return docRangeWorkPGNoByteStage(bound, fixture, document.Hash)
		}
		if released == 0 {
			if readCompleted.Load() || permit.resumeWait.Load() != resumeBefore {
				return errors.New("opaque decoder released its reservation before its terminal quantum")
			}
			if complete || resources.HeapBytes >= 2<<20 || resources.SpoolBytes != int64(len(stored)+len(canonical)) {
				return fmt.Errorf("closed decoder did not leave only stored and decoded spools before release: %+v", resources)
			}
			released = step
			return docRangeWorkPGNoByteStage(bound, fixture, document.Hash)
		}
		if !readCompleted.Load() || permit.resumeWait.Load() <= resumeBefore {
			return errors.New("decoder release did not restore foreground progress before proof")
		}
		if step == released+1 {
			if complete || resources.KeepPermit || resources.SpoolBytes != int64(len(canonical)) || resources.HeapBytes >= 2<<20 {
				return fmt.Errorf("stored-source release retained decoder state or began proof before yielding: %+v", resources)
			}
			return docRangeWorkPGNoByteStage(bound, fixture, document.Hash)
		}
		return nil
	}
	if err := fixture.service.ProcessDocRangeJobs(ctx, 1); err != nil {
		test.Fatal(err)
	}
	if pinnedSteps < 2 || released <= firstPinned || !readCompleted.Load() || probe.published != 1 || probe.closed != 1 {
		test.Fatal("compressed work did not keep then release its decoder reservation", pinnedSteps, firstPinned, released, probe)
	}
	select {
	case err := <-foreground:
		if err != nil {
			test.Fatal(err)
		}
	case <-ctx.Done():
		test.Fatal("foreground did not resume after decoder release", ctx.Err())
	}
	assertResourcesReleased(test, gate)
	assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
}
