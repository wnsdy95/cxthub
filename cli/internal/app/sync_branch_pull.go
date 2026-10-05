package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func (s *SyncRepoService) pullCapabilities(ctx context.Context, repo string) (outbound.PullCapabilities, error) {
	if remote, ok := s.remote.(outbound.RemotePullCapabilities); ok {
		return remote.PullCapabilities(ctx, repo)
	}
	protocol, err := s.remoteContextProtocol(ctx, repo)
	return outbound.PullCapabilities{ContextProtocol: protocol}, err
}

// Materialize only this plan's inventory. Old observed nodes may remain cached
// for a later delta, but their edges cannot participate in the returned proof.
func reconstructBranchPull(ctx context.Context, plan domain.BranchPullPlan, previous, incoming []domain.Snapshot) ([]domain.Snapshot, error) {
	byID := map[domain.ContentHash]domain.Snapshot{}
	for _, snap := range previous {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if plan.SnapshotStates[snap.ID] == "" {
			continue
		}
		hash, err := domain.SnapshotStateHash(snap)
		if err != nil {
			return nil, err
		}
		if hash == plan.SnapshotStates[snap.ID] {
			byID[snap.ID] = snap
		}
	}
	seen := map[domain.ContentHash]bool{}
	for _, snap := range incoming {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hash, err := domain.SnapshotStateHash(snap)
		if err != nil || plan.SnapshotStates[snap.ID] != hash || seen[snap.ID] {
			return nil, domain.ErrHashMismatch
		}
		seen[snap.ID] = true
		byID[snap.ID] = snap
	}
	out := make([]domain.Snapshot, 0, len(plan.SnapshotIndex))
	settings := map[domain.ContentHash]string{}
	for _, id := range plan.SnapshotIndex {
		snap, ok := byID[id]
		if !ok || snap.ID != snap.DocHash || snap.RepoID != plan.RepoID || domain.ValidateOptionalContentHash(snap.MemoryHash) != nil {
			return nil, domain.ErrHashMismatch
		}
		out = append(out, snap)
		for _, setting := range []domain.BranchPullSettings{{Kind: "claude", Hash: snap.ClaudeSettings}, {Kind: "agents", Hash: snap.AgentsSettings}, {Kind: "codex", Hash: snap.CodexSettings}} {
			if setting.Hash == "" {
				continue
			}
			if domain.ValidateContentHash(setting.Hash) != nil || (settings[setting.Hash] != "" && settings[setting.Hash] != setting.Kind) {
				return nil, domain.ErrHashMismatch
			}
			settings[setting.Hash] = setting.Kind
		}
	}
	if len(settings) != len(plan.SettingsObjects) {
		return nil, domain.ErrHashMismatch
	}
	for _, setting := range plan.SettingsObjects {
		if settings[setting.Hash] != setting.Kind {
			return nil, domain.ErrHashMismatch
		}
	}
	state := map[domain.ContentHash]uint8{}
	var visit func(domain.ContentHash) error
	visit = func(id domain.ContentHash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if state[id] == 2 {
			return nil
		}
		if state[id] == 1 {
			return domain.ErrHashMismatch
		}
		snap, ok := byID[id]
		if !ok {
			return fmt.Errorf("%w: branch plan omitted snapshot %s", domain.ErrHashMismatch, id)
		}
		state[id] = 1
		for _, parent := range snap.ReachabilityParents() {
			if err := visit(parent); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for _, snap := range out {
		if err := visit(snap.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}
