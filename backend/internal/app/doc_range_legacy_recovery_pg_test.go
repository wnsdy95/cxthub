//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docRangeLegacyRecoveryPGVerification struct {
	context.Context
	preparation atomic.Bool
	step        atomic.Bool
	event       atomic.Bool
}

func (ctx *docRangeLegacyRecoveryPGVerification) Err() error {
	if !ctx.preparation.Load() || !ctx.step.Load() || !ctx.event.Load() {
		var callers [32]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		for {
			frame, more := frames.Next()
			switch {
			case strings.HasSuffix(frame.Function, "domain.NewLegacyByteSourcePreparation"):
				ctx.preparation.Store(true)
			case strings.HasSuffix(frame.Function, "domain.(*LegacyByteSourcePreparation).Step"):
				ctx.step.Store(true)
			case strings.HasSuffix(frame.Function, "domain.verifyCanonicalEvent"):
				ctx.event.Store(true)
			}
			if !more {
				break
			}
		}
	}
	return ctx.Context.Err()
}

type docRangeLegacyRecoveryPGObservation struct {
	job domain.DocRangeIndexJob
	at  time.Time
}

type docRangeLegacyRecoveryPGStore struct {
	*store.PostgresStore
	afterStep     func(context.Context, *docRangeLegacyRecoveryPGWork, outbound.DocRangeWorkProgress) error
	beforePublish func(context.Context, *docRangeLegacyRecoveryPGWork) error
	closeFailure  error
	works         []*docRangeLegacyRecoveryPGWork
	publications  int
	finishCalls   int
	finishes      int
	finished      chan docRangeLegacyRecoveryPGObservation
	resume        <-chan struct{}
	closed        chan *docRangeLegacyRecoveryPGWork
}

type docRangeLegacyRecoveryPGWork struct {
	outbound.PreparedDocRangeWork
	storage      *docRangeLegacyRecoveryPGStore
	job          domain.DocRangeIndexJob
	started      time.Time
	kind         string
	steps        int
	closes       int
	publishCalls int
	published    bool
	failure      error
	verification docRangeLegacyRecoveryPGVerification
}

var _ outbound.DocRangeWorkStore = (*docRangeLegacyRecoveryPGStore)(nil)
var _ outbound.DocRangeJobStore = (*docRangeLegacyRecoveryPGStore)(nil)
var _ outbound.PreparedDocRangeWork = (*docRangeLegacyRecoveryPGWork)(nil)

func (storage *docRangeLegacyRecoveryPGStore) PrepareDocRangeWork(ctx context.Context, job domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
	started := time.Now().UTC()
	prepared, err := storage.PostgresStore.PrepareDocRangeWork(ctx, job)
	if err != nil || prepared == nil {
		return prepared, err
	}
	work := &docRangeLegacyRecoveryPGWork{PreparedDocRangeWork: prepared, storage: storage, job: job, started: started,
		kind: fmt.Sprintf("%T", prepared), verification: docRangeLegacyRecoveryPGVerification{Context: ctx}}
	storage.works = append(storage.works, work)
	resources, err := prepared.RetainedResources()
	if err != nil {
		return work, err
	}
	if resources.SpoolBytes != 0 || resources.HeapBytes <= 0 || resources.HeapBytes > 128<<10 || resources.KeepPermit {
		return work, fmt.Errorf("worker preparation did not leave bounded empty capture/recovery state: %+v", resources)
	}
	if err := prepared.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		return work, fmt.Errorf("new worker handle published before fresh verification: %v", err)
	}
	return work, nil
}

func (work *docRangeLegacyRecoveryPGWork) Step(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	bound := ctx
	if work.kind == "*store.preparedLegacyRecoveryWork" {
		bound = &work.verification
	}
	progress, err := work.PreparedDocRangeWork.Step(bound, job)
	if err == nil {
		work.steps++
		if progress != outbound.DocRangeWorkPending && progress != outbound.DocRangeWorkPublishable {
			err = fmt.Errorf("legacy recovery bypassed bounded worker preparation: %d", progress)
		} else if work.storage.afterStep != nil {
			err = work.storage.afterStep(ctx, work, progress)
		}
	}
	work.failure = err
	return progress, err
}

func (work *docRangeLegacyRecoveryPGWork) Publish(ctx context.Context, job domain.DocRangeIndexJob) error {
	work.publishCalls++
	if work.storage.beforePublish != nil {
		if err := work.storage.beforePublish(ctx, work); err != nil {
			work.failure = err
			return err
		}
	}
	work.failure = work.PreparedDocRangeWork.Publish(ctx, job)
	if work.failure == nil {
		work.published = true
		work.storage.publications++
	}
	return work.failure
}

func (work *docRangeLegacyRecoveryPGWork) Close() error {
	err := work.PreparedDocRangeWork.Close()
	work.closes++
	if work.published {
		err = errors.Join(err, work.storage.closeFailure)
	}
	if work.storage.closed != nil {
		work.storage.closed <- work
	}
	return err
}

func (storage *docRangeLegacyRecoveryPGStore) FinishDocRangeJob(ctx context.Context, job domain.DocRangeIndexJob, now time.Time) error {
	storage.finishCalls++
	if err := storage.PostgresStore.FinishDocRangeJob(ctx, job, now); err != nil {
		return err
	}
	storage.finishes++
	if storage.finished != nil {
		select {
		case storage.finished <- docRangeLegacyRecoveryPGObservation{job: job, at: now}:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-storage.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
