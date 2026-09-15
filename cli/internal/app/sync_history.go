package app

import (
	"context"
	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
	"reflect"
)

// A name-only PR lookup cannot distinguish an old merged branch from a newer
// task using its name. Preserve both until an exact PR source binding exists.
func (s *SyncRepoService) ResolveRemotePRBranch(ctx context.Context, in inbound.SyncInput, branch string) (domain.Ref, error) {
	ref, err := s.ResolveRemoteBranch(ctx, in, branch)
	if err != nil {
		return ref, err
	}
	if history, ok := s.store.(outbound.HistoryStore); ok {
		events, err := history.ListHistoryEvents(ctx, ref.RepoID)
		if err != nil {
			return ref, err
		}
		bindings, err := domain.ProjectContextBranches(events)
		if err != nil {
			return ref, err
		}
		if bindings.Released[branch] != "" {
			return ref, fmt.Errorf("%w: PR source branch %q was renamed, archived, or reused; an exact historical source binding is required", domain.ErrSyncConflict, branch)
		}
	}
	return ref, nil
}

func (s *SyncRepoService) pushHistory(ctx context.Context, repoID string) error {
	local, ok := s.store.(outbound.HistoryStore)
	if !ok {
		return nil
	}
	events, err := local.ListHistoryEvents(ctx, repoID)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	remote, ok := s.remote.(outbound.RemoteHistory)
	if !ok {
		return fmt.Errorf("server does not support durable context history; operations remain local")
	}
	accepted, err := remote.PullHistoryEvents(ctx, repoID)
	if err != nil {
		return err
	}
	byID := map[string]domain.HistoryEvent{}
	for _, e := range accepted {
		byID[e.ID] = e
	}
	for _, e := range events {
		if existing, ok := byID[e.ID]; ok {
			if !reflect.DeepEqual(existing, e) {
				return domain.ErrHashMismatch
			}
			continue
		}
		if e.Kind == "advance" {
			// The previous local tip may never have been pushed. Publish that
			// prerequisite only as a normal fast-forward before the retained
			// continuation CAS. Concurrent server work is never force-replaced.
			man, err := s.remote.RemoteManifest(ctx, repoID)
			if err != nil {
				return err
			}
			var target domain.ContentHash
			for _, ref := range man.Refs {
				if ref.Kind == domain.RefBranch && ref.Name == e.Branch {
					target = ref.Target
					break
				}
			}
			if target != e.Source {
				if !s.isAncestor(ctx, target, e.Source) {
					return fmt.Errorf("history %s awaits reconciliation with concurrent server work: %w", e.ID, domain.ErrSyncConflict)
				}
				base := domain.Ref{RepoID: repoID, Kind: domain.RefBranch, Name: e.Branch, Target: e.Source}
				if err := s.remote.Push(ctx, repoID, nil, nil, []domain.Ref{base}, false, false); err != nil {
					return err
				}
			}
		}
		if err := remote.PushHistoryEvent(ctx, e); err != nil {
			return fmt.Errorf("history %s (%s) remains pending: %w", e.ID, e.Kind, err)
		}
	}
	return nil
}

func (s *SyncRepoService) readRemoteHistory(ctx context.Context, repoID string) ([]domain.HistoryEvent, error) {
	remote, ok := s.remote.(outbound.RemoteHistory)
	if !ok {
		return nil, nil
	}
	events, err := remote.PullHistoryEvents(ctx, repoID)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		if e.RepoID != repoID {
			return nil, domain.ErrHashMismatch
		}
		if err := domain.ValidateHistoryEvent(e); err != nil {
			return nil, err
		}
	}
	return events, nil
}

func (s *SyncRepoService) storeRemoteHistory(ctx context.Context, events []domain.HistoryEvent) error {
	ordered, err := domain.OrderHistoryEvents(events)
	if err != nil {
		return err
	}
	events = ordered
	local, ok := s.store.(outbound.HistoryStore)
	if !ok && len(events) > 0 {
		return fmt.Errorf("local context history storage unavailable")
	}
	for _, e := range events {
		for _, id := range []domain.ContentHash{e.Source, e.Target, e.SharedTarget, e.MemorySource} {
			if id == "" {
				continue
			}
			snap, err := s.store.GetSnapshot(ctx, id)
			if err != nil {
				return err
			}
			if snap.RepoID != e.RepoID {
				return domain.ErrHashMismatch
			}
		}
	}
	// Validate the complete incoming set before publishing any retained roots.
	for _, e := range events {
		if err := local.PutHistoryEvent(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
