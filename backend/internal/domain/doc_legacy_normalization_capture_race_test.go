package domain

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestLegacyNormalizationCaptureW87ConcurrentNativeStart(test *testing.T) {
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{"kind":"message","text":"retained"}`, nil, func(native *legacyNativeObject) bool {
		return native.phase == legacyNativeObjectStart
	})
	storage := legacyNativeAnyW78Store(test)
	probe, err := newLegacyNativeObjectCheckpoint(context.Background(), native, scanner, request.Binding, source, storage.io(), legacyNativeEventW82FenceBudget)
	if err != nil {
		test.Fatal(err)
	}
	probe.close()
	start := make(chan struct{})
	failures := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		for attempt := 0; attempt < 4096; attempt++ {
			if err := native.start(0, source.Bytes); err != nil && !errors.Is(err, ErrLegacyNormalizationBusy) {
				failures <- err
				return
			}
		}
	}()
	close(start)
	completed := 0
	for attempt := 0; attempt < 8192 && completed < 64; attempt++ {
		work, err := newLegacyNativeObjectCheckpoint(context.Background(), native, scanner, request.Binding, source, storage.io(), legacyNativeEventW82FenceBudget)
		if errors.Is(err, ErrLegacyNormalizationBusy) {
			continue
		}
		if err != nil {
			workers.Wait()
			test.Fatal(err)
		}
		if err := native.start(0, source.Bytes); !errors.Is(err, ErrLegacyNormalizationBusy) {
			work.close()
			workers.Wait()
			test.Fatal("native start entered a captured object", err)
		}
		if err := work.checkSource(); err != nil {
			work.close()
			workers.Wait()
			test.Fatal("captured state changed", err)
		}
		work.close()
		completed++
	}
	workers.Wait()
	select {
	case failure := <-failures:
		test.Fatal(failure)
	default:
	}
	if completed != 64 || native.mutation.activity.Load() != 0 || native.mutation.capture.Load() != nil {
		test.Fatal("capture did not finish within its finite schedule", completed, native.mutation.activity.Load())
	}
}
