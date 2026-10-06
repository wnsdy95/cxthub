package app

import (
	"context"
	"fmt"
	"sort"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ inbound.IncomingContextDiscovery = (*SyncRepoService)(nil)

func (s *SyncRepoService) DiscoverIncomingSnapshots(ctx context.Context, in inbound.SyncInput) ([]domain.Snapshot, bool, error) {
	repo, err := s.repoID(ctx, in)
	if err != nil {
		return nil, false, err
	}
	capabilities, err := s.pullCapabilities(ctx, repo)
	if err != nil {
		return nil, false, err
	}
	if capabilities.ContextProtocol < 0 || capabilities.ContextProtocol > 1 || capabilities.BranchPlanVersion < 0 || capabilities.BranchPlanVersion > domain.BranchPullVersion {
		return nil, false, domain.ErrSyncConflict
	}
	if capabilities.BranchPlanVersion == 0 {
		return nil, false, nil
	}
	reader, ok := s.remote.(outbound.RemoteSnapshotCatalog)
	_, remoteScope := s.remote.(outbound.ScopedBranchRemotePull)
	_, localScope := s.store.(outbound.ScopedRemoteObservationStore)
	if !ok || !remoteScope || !localScope {
		return nil, false, fmt.Errorf("selected incoming context discovery is unavailable in this client adapter")
	}
	remote, err := reader.ReadSnapshotCatalog(ctx, repo)
	if err != nil {
		return nil, false, err
	}
	local, err := s.store.ListSnapshots(ctx, repo, "")
	if err != nil {
		return nil, false, err
	}
	// FetchOnly preserves metadata already present locally. Match that behavior
	// for discovery, including local unpublished labels and rewrite aliases.
	byID := make(map[domain.ContentHash]domain.Snapshot, len(remote)+len(local))
	for _, snap := range remote {
		if err := validateSnapshotObject(snap); err != nil {
			return nil, false, err
		}
		if snap.RepoID != repo {
			return nil, false, domain.ErrHashMismatch
		}
		if _, duplicate := byID[snap.ID]; duplicate {
			return nil, false, domain.ErrHashMismatch
		}
		byID[snap.ID] = snap
	}
	for _, snap := range local {
		byID[snap.ID] = snap
	}
	out := make([]domain.Snapshot, 0, len(byID))
	for _, snap := range byID {
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, true, ctx.Err()
}
