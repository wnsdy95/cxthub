package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestBuildDocRangeWorkLateCompatibilityClosesBeforeDirectFallback(tests *testing.T) {
	fallbackFailure := errors.New("compatibility failed")
	for _, scenario := range []struct {
		name        string
		pending     bool
		fallbackErr error
	}{
		{name: "immediate"},
		{name: "pending-then-compatibility", pending: true},
		{name: "compatibility-error", pending: true, fallbackErr: fallbackFailure},
	} {
		tests.Run(scenario.name, func(test *testing.T) {
			ctx := context.Background()
			job := domain.DocRangeIndexJob{RepoID: hh(test.Name()), DocHash: hh("late-compatibility"), Version: 17}
			var calls []string
			prepared := &preparedDocRangeWorkStub{}
			prepared.step = func(bound context.Context, selected domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
				if bound != ctx || selected != job || prepared.closes != 0 {
					test.Fatal("late compatibility changed binding or stepped closed work")
				}
				if scenario.pending && prepared.steps == 1 {
					calls = append(calls, "pending")
					return outbound.DocRangeWorkPending, nil
				}
				calls = append(calls, "compatibility-required")
				return outbound.DocRangeWorkCompatibilityRequired, nil
			}
			prepared.resources = func() (outbound.RootPublicationResources, error) {
				calls = append(calls, "resources")
				return outbound.RootPublicationResources{HeapBytes: 4096, SpoolBytes: 128}, nil
			}
			prepared.close = func() error {
				calls = append(calls, "close")
				return nil
			}
			storage := preparingDocRangeWorkStore{
				docRangeWorkJobStore: docRangeWorkJobStore{build: func(context.Context, domain.DocRangeIndexJob) error {
					test.Fatal("late compatibility reentered generic preparation")
					return domain.ErrIntegrity
				}},
				prepare: func(bound context.Context, selected domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
					if bound != ctx || selected != job {
						test.Fatal("late compatibility preparation changed binding")
					}
					calls = append(calls, "prepare")
					return prepared, nil
				},
				compatibility: func(bound context.Context, selected domain.DocRangeIndexJob) error {
					if bound != ctx || selected != job || prepared.closes != 1 || prepared.publications != 0 {
						test.Fatal("fallback preceded private cleanup or changed binding")
					}
					calls = append(calls, "fallback")
					return scenario.fallbackErr
				},
			}
			service := NewService(yieldingPreparationMetadata{}, nil, nil, nil, nil)
			published, err := service.buildDocRangeWork(ctx, storage, job)
			want := []string{"prepare"}
			wantSteps, wantResources := 1, 0
			if scenario.pending {
				want = append(want, "pending", "resources")
				wantSteps, wantResources = 2, 1
			}
			want = append(want, "compatibility-required", "close", "fallback")
			if !reflect.DeepEqual(calls, want) || prepared.steps != wantSteps || prepared.resourceChecks != wantResources ||
				prepared.closes != 1 || prepared.publications != 0 || published != (scenario.fallbackErr == nil) || !errors.Is(err, scenario.fallbackErr) {
				test.Fatal("late compatibility changed its ordered, single-close lifecycle", calls, want, published, err, prepared)
			}
		})
	}
}

func TestBuildDocRangeWorkLateCompatibilityFailsClosed(tests *testing.T) {
	stepFailure, cleanupFailure := errors.New("step failed"), errors.New("cleanup failed")
	for _, scenario := range []struct {
		name        string
		progress    outbound.DocRangeWorkProgress
		stepErr     error
		closeErr    error
		cancelStep  bool
		cancelClose bool
		wantErr     error
	}{
		{name: "close-failure", progress: outbound.DocRangeWorkCompatibilityRequired, closeErr: cleanupFailure, wantErr: cleanupFailure},
		{name: "canceled-step", progress: outbound.DocRangeWorkCompatibilityRequired, cancelStep: true, wantErr: context.Canceled},
		{name: "canceled-close", progress: outbound.DocRangeWorkCompatibilityRequired, cancelClose: true, wantErr: context.Canceled},
		{name: "zero-outcome", wantErr: domain.ErrIntegrity},
		{name: "unknown-outcome", progress: 255, wantErr: domain.ErrIntegrity},
		{name: "pending-error", progress: outbound.DocRangeWorkPending, stepErr: stepFailure, wantErr: stepFailure},
		{name: "publishable-error", progress: outbound.DocRangeWorkPublishable, stepErr: stepFailure, wantErr: stepFailure},
		{name: "compatibility-error", progress: outbound.DocRangeWorkCompatibilityRequired, stepErr: stepFailure, wantErr: stepFailure},
		{name: "error-and-close-failure", progress: outbound.DocRangeWorkCompatibilityRequired, stepErr: stepFailure, closeErr: cleanupFailure, wantErr: stepFailure},
		{name: "invalid-and-close-failure", closeErr: cleanupFailure, wantErr: domain.ErrIntegrity},
	} {
		tests.Run(scenario.name, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls []string
			prepared := &preparedDocRangeWorkStub{
				step: func(context.Context, domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
					calls = append(calls, "step")
					if scenario.cancelStep {
						cancel()
					}
					return scenario.progress, scenario.stepErr
				},
				close: func() error {
					calls = append(calls, "close")
					if scenario.cancelClose {
						cancel()
					}
					return scenario.closeErr
				},
			}
			storage := preparingDocRangeWorkStore{
				docRangeWorkJobStore: docRangeWorkJobStore{build: func(context.Context, domain.DocRangeIndexJob) error {
					test.Fatal("failed private work reentered generic preparation")
					return domain.ErrIntegrity
				}},
				prepare: func(context.Context, domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
					calls = append(calls, "prepare")
					return prepared, nil
				},
				compatibility: func(context.Context, domain.DocRangeIndexJob) error {
					test.Fatal("failed or canceled private work entered compatibility fallback")
					return domain.ErrIntegrity
				},
			}
			service := NewService(yieldingPreparationMetadata{}, nil, nil, nil, nil)
			published, err := service.buildDocRangeWork(ctx, storage, domain.DocRangeIndexJob{})
			if published || !errors.Is(err, scenario.wantErr) || (scenario.closeErr != nil && !errors.Is(err, scenario.closeErr)) ||
				!reflect.DeepEqual(calls, []string{"prepare", "step", "close"}) || prepared.steps != 1 ||
				prepared.closes != 1 || prepared.resourceChecks != 0 || prepared.publications != 0 {
				test.Fatal("invalid progress, error, cancellation, or cleanup failure advanced work", calls, published, err, prepared)
			}
		})
	}
}
