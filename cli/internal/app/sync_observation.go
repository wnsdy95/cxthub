package app

import (
	"context"
	"fmt"
	"sort"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// WithRemoteIdentity binds observation and delta cursors to the resolved,
// credential-free endpoint identity. It returns a copy for per-command use.
func (s *SyncRepoService) WithRemoteIdentity(identity string) *SyncRepoService {
	copy := *s
	copy.remoteIdentity = identity
	return &copy
}

func (s *SyncRepoService) observationRemote() string {
	if s.remoteIdentity != "" {
		return s.remoteIdentity
	}
	if remote, ok := s.remote.(outbound.SyncRemoteIdentity); ok && remote.SyncRemoteIdentity() != "" {
		return remote.SyncRemoteIdentity()
	}
	return "configured"
}

func (s *SyncRepoService) readObservation(ctx context.Context, repo string) (outbound.RemoteObservation, error) {
	if store, ok := s.store.(outbound.RemoteObservationStore); ok {
		return store.ReadRemoteObservation(ctx, repo, s.observationRemote())
	}
	return outbound.RemoteObservation{Version: 1, RepoID: repo, Remote: s.observationRemote()}, nil
}

func mergeObservedSnapshots(previous, incoming []domain.Snapshot) []domain.Snapshot {
	byID := make(map[domain.ContentHash]domain.Snapshot, len(previous)+len(incoming))
	for _, snap := range previous {
		byID[snap.ID] = snap
	}
	for _, snap := range incoming {
		byID[snap.ID] = snap
	}
	out := make([]domain.Snapshot, 0, len(byID))
	for _, snap := range byID {
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *SyncRepoService) recordObservation(ctx context.Context, previous outbound.RemoteObservation, snaps []domain.Snapshot, refs []domain.Ref, history []domain.HistoryEvent) error {
	store, ok := s.store.(outbound.RemoteObservationStore)
	if !ok {
		return fmt.Errorf("remote observation storage unavailable; fetch cannot safely retain unapplied metadata")
	}
	next := previous
	next.Snapshots = mergeObservedSnapshots(previous.Snapshots, snaps)
	next.Refs, next.History = refs, history
	if err := store.CompareAndSwapRemoteObservation(ctx, previous.Revision, next); err != nil {
		return fmt.Errorf("record remote observation: %w", err)
	}
	return nil
}
