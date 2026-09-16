package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// ResolvePRSourcePosition resolves the recorded memory of a completed PR's
// frozen source. It does not select a worktree or read mutable memory pointers.
// The result is the causal maximum among exact pinned source observations;
// receipts without an observation ID cannot identify a publication-time cutoff.
func (s *ContextHistoryService) ResolvePRSourcePosition(ctx context.Context, receipt domain.HistoryEvent) (domain.WorkingPosition, error) {
	var zero domain.WorkingPosition
	if err := domain.ValidateHistoryEvent(receipt); err != nil {
		return zero, fmt.Errorf("invalid PR receipt: %w", err)
	}
	if receipt.Kind != "pr-merge" || !receipt.PRCompleted {
		return zero, fmt.Errorf("%w: completed PR receipt required", domain.ErrSyncConflict)
	}
	// Completion.Target may contain later work. Validate only the frozen source,
	// with memory explicitly pinned so validation cannot inherit a mutable pointer.
	proof := domain.HistoryEvent{ID: receipt.ID, RepoID: receipt.RepoID, BranchID: receipt.SourceBranchID,
		Branch: receipt.PR.HeadBranch, Kind: "position", Source: receipt.Source, Target: receipt.Source,
		GitAfter: receipt.PR.HeadSHA, MemoryPinned: true, CreatedAt: receipt.CreatedAt}
	if _, err := s.ValidateHistorySource(ctx, proof); err != nil {
		return zero, err
	}
	events, err := s.history.ListHistoryEvents(ctx, receipt.RepoID)
	if err != nil {
		return zero, err
	}
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
	var owner domain.ContentHash
	for _, e := range events {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if e.Kind == "publish" || e.Kind == "pr-merge" || !e.MemoryPinned || e.RepoID != receipt.RepoID ||
			e.BranchID != receipt.SourceBranchID || e.GitAfter != receipt.PR.HeadSHA || e.Target != receipt.Source {
			continue
		}
		if _, err := s.ValidateHistorySource(ctx, e); err != nil {
			return zero, fmt.Errorf("PR source observation %s: %w", e.ID, err)
		}
		p := domain.WorkingPosition{RepoID: receipt.RepoID, Branch: receipt.Branch, BranchID: receipt.BranchID,
			GitCommit: receipt.PR.MergeSHA, Snapshot: receipt.Source,
			MemoryHash: e.MemoryHash, MemorySource: e.MemorySource, MemoryPinned: true}
		if e.MemoryHash != "" {
			d, err := load(e.MemoryHash)
			if err != nil {
				return zero, err
			}
			if owner != "" && owner != d.SnapshotID {
				return zero, fmt.Errorf("%w: PR source memory observations have different owners", domain.ErrSyncConflict)
			}
			owner = d.SnapshotID
			// A birth can pin memory owned by its Source without MemorySource.
			// Carry that verified ownership explicitly into the selected target.
			if p.MemorySource == "" && d.SnapshotID != p.Snapshot {
				p.MemorySource = d.SnapshotID
			}
		}
		if old, ok := candidates[p.MemoryHash]; ok && old.MemorySource != p.MemorySource {
			return zero, fmt.Errorf("%w: PR source memory provenance is ambiguous", domain.ErrSyncConflict)
		}
		candidates[p.MemoryHash] = p
	}
	if len(candidates) == 0 {
		return zero, fmt.Errorf("%w: completed PR source has no exact pinned memory observation", domain.ErrNotFound)
	}
	if empty, ok := candidates[""]; ok {
		if len(candidates) != 1 {
			return zero, fmt.Errorf("%w: explicit empty and nonempty PR source memories have no recorded selection order", domain.ErrSyncConflict)
		}
		return empty, nil
	}
	chains := map[domain.ContentHash]map[domain.ContentHash]bool{}
	for hash := range candidates {
		seen := map[domain.ContentHash]bool{}
		for id := hash; id != ""; {
			if seen[id] {
				return zero, fmt.Errorf("%w: cyclic PR source memory history", domain.ErrHashMismatch)
			}
			seen[id] = true
			d, err := load(id)
			if err != nil {
				return zero, fmt.Errorf("PR source memory history %s: %w", id, err)
			}
			if d.SnapshotID != owner {
				return zero, fmt.Errorf("%w: PR source memory parent belongs to another snapshot", domain.ErrHashMismatch)
			}
			id = d.PreviousMemoryHash
		}
		chains[hash] = seen
	}
	for hash, p := range candidates {
		dominates := true
		for other := range candidates {
			if !chains[hash][other] {
				dominates = false
				break
			}
		}
		if dominates {
			selected := proof
			selected.MemoryHash, selected.MemorySource = p.MemoryHash, p.MemorySource
			if _, err := s.ValidateHistorySource(ctx, selected); err != nil {
				return zero, err
			}
			return p, nil
		}
	}
	return zero, fmt.Errorf("%w: PR source memory observations have divergent causal histories", domain.ErrSyncConflict)
}
