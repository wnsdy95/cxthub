//go:build postgres

package app

import (
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

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
