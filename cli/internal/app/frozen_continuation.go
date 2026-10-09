package app

import (
	"context"
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// RecordFrozenContinuation pins the completed predecessor at this attempt's
// code point. It records selection evidence without rewriting memory objects
// or snapshot attachments, including when all providers are absent.
func (s *SaveSessionService) RecordFrozenContinuation(ctx context.Context, cwd string, p domain.CaptureAttempt) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Predecessor == nil {
		return nil
	}
	if p.Version != 2 || !p.InputsReady {
		return domain.ErrHashMismatch
	}
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return err
	}
	if repo.ID != p.Proof.RepoID {
		return domain.ErrSelectionChanged
	}
	retention, ok := s.store.(outbound.ObjectRetention)
	if !ok {
		return fmt.Errorf("memory retention unavailable")
	}
	history, ok := s.store.(outbound.HistoryStore)
	if !ok {
		return fmt.Errorf("capture history unavailable")
	}
	return retention.WithObjectsRetained(ctx, func() error {
		if err := s.verifyFrozenPredecessor(ctx, p); err != nil {
			return err
		}
		prior := p.PredecessorObservation
		continuation := p.Proof
		continuation.ID = domain.CaptureContinuationObservationID(p.Proof.ID)
		continuation.Source, continuation.Target = prior.Target, prior.Target
		continuation.MemoryHash, continuation.MemorySource, continuation.MemoryPinned = prior.MemoryHash, prior.MemorySource, true
		service := NewContextHistoryService(s.store, history)
		if _, err := service.ValidateHistorySource(ctx, continuation); err != nil {
			return err
		}

		baseline := p.Proof
		baseline.ID = domain.CaptureBaselineObservationID(p.Proof.ID)
		baseline.Source, baseline.Target = p.Initial, p.Initial
		root := continuation
		root.ID = domain.CaptureBaselineObservationID(continuation.ID)
		root.MemorySelectionParent = baseline.ID
		if domain.IsInitialMemorySelection(baseline, root) {
			// The baseline must be the exact durable record retained at B's
			// admission, not a newly inferred empty or inherited selection.
			events, err := history.ListHistoryEvents(ctx, repo.ID)
			if err != nil {
				return err
			}
			found := false
			for _, event := range events {
				if event.ID == baseline.ID {
					if !reflect.DeepEqual(event, baseline) {
						return domain.ErrHashMismatch
					}
					found = true
				}
			}
			if !found {
				return fmt.Errorf("frozen capture baseline observation is missing: %w", domain.ErrNotFound)
			}
			if _, err := service.ValidateHistorySource(ctx, baseline); err != nil {
				return err
			}
			root.MemoryHash, err = s.frozenMemoryRoot(ctx, continuation.Target, continuation.MemoryHash)
			if err != nil {
				return err
			}
			root.MemorySource = root.Target
			if _, err := service.ValidateHistorySource(ctx, root); err != nil {
				return err
			}
			if err := service.RecordHistory(ctx, root); err != nil {
				return err
			}
		}
		return service.RecordHistory(ctx, continuation)
	})
}
