package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// recordedMemorySelection chooses a causal maximum from already validated
// pinned observations of one snapshot. Neither wall clocks nor its mutable
// memory attachment can order divergent or explicitly empty observations.
func (s *ContextHistoryService) recordedMemorySelection(ctx context.Context, positions []domain.WorkingPosition) (domain.WorkingPosition, error) {
	var zero domain.WorkingPosition
	memories := map[domain.ContentHash]domain.MemoryDigest{}
	load := func(hash domain.ContentHash) (domain.MemoryDigest, error) {
		if err := ctx.Err(); err != nil {
			return domain.MemoryDigest{}, err
		}
		if d, ok := memories[hash]; ok {
			return d, nil
		}
		if err := domain.ValidateContentHash(hash); err != nil {
			return domain.MemoryDigest{}, err
		}
		d, err := s.store.GetMemory(ctx, hash)
		if err != nil {
			return d, err
		}
		actual, err := domain.MemoryDigestHash(d)
		if err != nil {
			return d, err
		}
		if actual != hash || domain.ValidateContentHash(d.SnapshotID) != nil || domain.ValidateOptionalContentHash(d.PreviousMemoryHash) != nil {
			return d, domain.ErrHashMismatch
		}
		memories[hash] = d
		return d, nil
	}

	candidates := map[domain.ContentHash]domain.WorkingPosition{}
	var owner, snapshot domain.ContentHash
	for _, p := range positions {
		if !p.MemoryPinned || p.Snapshot == "" {
			return zero, domain.ErrHashMismatch
		}
		if snapshot != "" && snapshot != p.Snapshot {
			return zero, domain.ErrSyncConflict
		}
		snapshot = p.Snapshot
		if p.MemoryHash != "" {
			d, err := load(p.MemoryHash)
			if err != nil {
				return zero, err
			}
			if owner != "" && owner != d.SnapshotID {
				return zero, fmt.Errorf("%w: recorded memories have different owners", domain.ErrSyncConflict)
			}
			if p.MemorySource != "" && p.MemorySource != d.SnapshotID {
				return zero, domain.ErrHashMismatch
			}
			owner = d.SnapshotID
			p.MemorySource = d.SnapshotID
			if d.SnapshotID == p.Snapshot {
				p.MemorySource = ""
			}
		}
		if old, ok := candidates[p.MemoryHash]; ok && old.MemorySource != p.MemorySource {
			return zero, fmt.Errorf("%w: recorded memory provenance is ambiguous", domain.ErrSyncConflict)
		}
		candidates[p.MemoryHash] = p
	}
	if len(candidates) == 0 {
		return zero, fmt.Errorf("%w: no exact pinned memory observation", domain.ErrNotFound)
	}
	if empty, ok := candidates[""]; ok {
		if len(candidates) != 1 {
			return zero, fmt.Errorf("%w: empty and nonempty recorded memories have no causal selection order", domain.ErrSyncConflict)
		}
		return empty, nil
	}
	for hash, p := range candidates {
		seen := map[domain.ContentHash]bool{}
		for id := hash; id != ""; {
			if seen[id] {
				return zero, fmt.Errorf("%w: cyclic recorded memory history", domain.ErrHashMismatch)
			}
			seen[id] = true
			d, err := load(id)
			if err != nil {
				return zero, err
			}
			if d.SnapshotID != owner {
				return zero, domain.ErrHashMismatch
			}
			id = d.PreviousMemoryHash
		}
		dominates := true
		for other := range candidates {
			if !seen[other] {
				dominates = false
				break
			}
		}
		if dominates {
			return p, nil
		}
	}
	return zero, fmt.Errorf("%w: recorded memories have divergent causal histories", domain.ErrSyncConflict)
}
