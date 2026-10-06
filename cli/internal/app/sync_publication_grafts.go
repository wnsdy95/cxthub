package app

import (
	"context"
	"fmt"
	"slices"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// A selected event after any excluded head cannot be acknowledged by the global
// FIFO. Fail before sends rather than skipping that head or broadening scope.
func publicationGraftPrefix(queue []domain.GraftQueueEvent, snaps []domain.Snapshot) ([]domain.GraftQueueEvent, error) {
	byID := map[domain.ContentHash]domain.Snapshot{}
	for _, snap := range snaps {
		byID[snap.ID] = snap
	}
	var prefix []domain.GraftQueueEvent
	excluded := false
	for _, event := range queue {
		snap, selected := byID[domain.ContentHash(event.Snapshot)]
		if !selected {
			excluded = true
			continue
		}
		if excluded {
			return nil, fmt.Errorf("%w: required graft lies behind excluded FIFO head", domain.ErrSyncConflict)
		}
		if event.Legacy || event.ExpectedSeq >= snap.GraftSeq {
			return nil, fmt.Errorf("%w: graft requires prior local reconciliation", domain.ErrSyncConflict)
		}
		for _, parent := range event.Parents {
			id := domain.ContentHash(parent)
			if _, ok := byID[id]; !ok || !slices.Contains(snap.ReachabilityParents(), id) {
				return nil, fmt.Errorf("%w: graft exceeds frozen dependency graph", domain.ErrSyncConflict)
			}
		}
		event.Parents = slices.Clone(event.Parents)
		prefix = append(prefix, event)
	}
	return prefix, nil
}

func (s *SyncRepoService) checkPublicationState(ctx context.Context, root string, snaps []domain.Snapshot, remaining []domain.GraftQueueEvent) error {
	return s.outbox.WithGrafts(ctx, root, func(q outbound.GraftQueueAccess) error {
		current, err := q.Load()
		if err != nil {
			return err
		}
		if len(current) < len(remaining) {
			return domain.ErrSyncConflict
		}
		for i, e := range remaining {
			if !sameGraftQueueEvent(current[i], e) {
				return fmt.Errorf("%w: planned graft prefix changed", domain.ErrSyncConflict)
			}
		}
		selected := map[string]bool{}
		for _, frozen := range snaps {
			selected[string(frozen.ID)] = true
			now, err := s.store.GetSnapshot(ctx, frozen.ID)
			if err != nil {
				return err
			}
			a, err := domain.SnapshotStateHash(now)
			if err != nil {
				return err
			}
			b, err := domain.SnapshotStateHash(frozen)
			if err != nil {
				return err
			}
			if a != b || now.ID != frozen.ID || now.DocHash != frozen.DocHash || !sameParents(now.Parents, frozen.Parents) {
				return fmt.Errorf("%w: frozen publication graph changed", domain.ErrSyncConflict)
			}
		}
		for _, e := range current[len(remaining):] {
			if selected[e.Snapshot] {
				return fmt.Errorf("%w: new selected graft requires replanning", domain.ErrSyncConflict)
			}
		}
		return nil
	})
}

// Every send uses a planned event. Both head checks are inside the existing
// queue lock, the network call is outside, and disappearance never counts as ack.
func (s *SyncRepoService) executePublicationGrafts(ctx context.Context, root, repo string, prefix []domain.GraftQueueEvent) error {
	for _, event := range prefix {
		head := func(q outbound.GraftQueueAccess, ack bool) error {
			current, err := q.Load()
			if err != nil {
				return err
			}
			if len(current) == 0 || !sameGraftQueueEvent(current[0], event) {
				return fmt.Errorf("%w: planned graft head changed", domain.ErrSyncConflict)
			}
			if ack {
				return q.Store(current[1:])
			}
			return nil
		}
		if err := s.outbox.WithGrafts(ctx, root, func(q outbound.GraftQueueAccess) error { return head(q, false) }); err != nil {
			return err
		}
		parents := make([]domain.ContentHash, 0, len(event.Parents))
		for _, p := range event.Parents {
			parents = append(parents, domain.ContentHash(p))
		}
		err := s.remote.GraftSnapshotParents(ctx, repo, domain.ContentHash(event.Snapshot), parents, event.ExpectedSeq)
		if err != nil {
			// No global conflict rebase/removal here: it could consume an unplanned tail.
			// The exact event remains durable and the stale plan cannot publish history.
			if terminalGraftConflict(err) {
				return fmt.Errorf("%w: planned graft conflicted; replan after reconciliation: %w", domain.ErrSyncConflict, err)
			}
			return err
		}
		if err = s.outbox.WithGrafts(ctx, root, func(q outbound.GraftQueueAccess) error { return head(q, true) }); err != nil {
			return err
		}
	}
	return nil
}
