package repositoryquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (queries *ContextQueries) finishContextQuery(ctx context.Context, repo domain.ContentHash, in domain.ContextSelection, out domain.ContextQueryView, full domain.RepositoryView, key contextSegmentCacheKey) (domain.ContextQueryView, error) {
	if out.StateHash == "" {
		basis, err := json.Marshal(struct {
			Version   int
			Selection domain.ContextSelection
			Position  domain.ContentHash
			Snapshots []domain.Snapshot
			History   []domain.HistoryEvent
		}{out.Version, in, out.Position, out.Snapshots, out.History})
		if err != nil {
			return out, err
		}
		out.StateHash = domain.HashContent(basis)
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	bindings := domain.ContextPublicationBindings(repo, full.History)
	semantic := out
	semantic.History, semantic.Semantics = full.History, full.Semantics
	var err error
	out.DeliveryStateHash, err = domain.ContextDeliveryStateHash(repo, key.origin, in, semantic, bindings)
	if err != nil {
		return out, err
	}
	if in.SegmentLimit == 0 {
		return out, nil
	}
	if in.SegmentOffset > len(out.Snapshots) {
		return out, fmt.Errorf("%w: segment offset exceeds selection", domain.ErrValidation)
	}
	type sourceBindings struct {
		SnapshotID domain.ContentHash
		Bindings   []domain.CommitContextBinding
	}
	receipts := make([]sourceBindings, 0, len(out.Snapshots))
	for _, snapshot := range out.Snapshots {
		receipts = append(receipts, sourceBindings{snapshot.ID, bindings[snapshot.ID]})
	}
	basis, err := json.Marshal(struct {
		Version      int
		Selection    domain.ContextSelection
		Position     domain.ContentHash
		Inclusion    *domain.BranchContext
		Snapshots    []domain.Snapshot
		Publications []sourceBindings
		Delivery     domain.ContentHash
	}{domain.ContextSegmentVersion, domain.ContextSelection{Branch: out.Branch, Scope: in.Scope, CodeCommit: in.CodeCommit}, out.Position, out.Inclusion, out.Snapshots, receipts, out.DeliveryStateHash})
	if err != nil {
		return out, err
	}
	state := domain.HashContent(basis)
	if in.SegmentStateHash != "" && in.SegmentStateHash != state {
		return out, fmt.Errorf("%w: context segments changed; restart without offset", domain.ErrConflict)
	}
	out.StateHash = state
	selected := make(map[domain.ContentHash]domain.Snapshot, len(out.Snapshots))
	selectedBindings := make(map[domain.ContentHash][]domain.CommitContextBinding, len(out.Snapshots))
	for _, snapshot := range out.Snapshots {
		selected[snapshot.ID] = snapshot
		if entries, ok := bindings[snapshot.ID]; ok {
			selectedBindings[snapshot.ID] = entries
		}
	}
	basisView := contextSegmentBasis{view: out, selected: selected, bindings: selectedBindings}
	if key.cacheable() {
		queries.cache.put(key, basisView)
		resolved := key
		resolved.position, resolved.branch = string(out.Position), out.Branch
		queries.cache.put(resolved, basisView)
	}
	return queries.contextSegmentPage(ctx, repo, in, basisView)
}

func (queries *ContextQueries) contextSegmentPage(ctx context.Context, repo domain.ContentHash, in domain.ContextSelection, basis contextSegmentBasis) (domain.ContextQueryView, error) {
	out, selected, bindings := basis.view, basis.selected, basis.bindings
	state := out.StateHash
	if in.SegmentStateHash != "" && in.SegmentStateHash != state {
		return out, fmt.Errorf("%w: context segments changed; restart without offset", domain.ErrConflict)
	}
	if in.SegmentOffset > len(out.Snapshots) {
		return out, domain.ErrValidation
	}
	indexes := map[domain.ContentHash]domain.DocReadIndex{}
	var indexOrder []domain.ContentHash
	read := func(hash domain.ContentHash) (domain.DocReadIndex, error) {
		if index, ok := indexes[hash]; ok {
			return index, nil
		}
		index, err := queries.indexes.ReadIndex(ctx, repo, hash)
		if err == nil {
			if len(indexOrder) == 2 {
				delete(indexes, indexOrder[0])
				indexOrder = indexOrder[1:]
			}
			indexes[hash] = index
			indexOrder = append(indexOrder, hash)
		}
		return index, err
	}
	end := min(in.SegmentOffset+in.SegmentLimit, len(out.Snapshots))
	page := domain.ContextSegmentPage{Version: domain.ContextSegmentVersion, StateHash: state, Offset: in.SegmentOffset, Next: -1, Total: len(out.Snapshots), Complete: end == len(out.Snapshots), Entries: []domain.ContextSegmentCoverage{}}
	if !page.Complete {
		page.Next = end
	}
	for _, snapshot := range out.Snapshots[in.SegmentOffset:end] {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if snapshot.RepoID != repo {
			return out, domain.ErrIntegrity
		}
		snapshotBindings := cloneContextBindings(bindings[snapshot.ID])
		index, err := read(snapshot.DocHash)
		if errors.Is(err, domain.ErrNotFound) {
			page.Entries = append(page.Entries, domain.ContextSegmentCoverage{SnapshotID: snapshot.ID, Kind: "unavailable", Reason: "source_unavailable", TotalEvents: -1, Bindings: append([]domain.CommitContextBinding{}, snapshotBindings...)})
			continue
		}
		if err != nil {
			return out, err
		}
		var base *domain.Snapshot
		var previous *domain.DocReadIndex
		if len(bindings[snapshot.ID]) > 0 && len(snapshot.Parents) == 1 && snapshot.Parents[0] != snapshot.ID {
			if candidate, ok := selected[snapshot.Parents[0]]; ok && len(bindings[candidate.ID]) > 0 {
				old, err := read(candidate.DocHash)
				if err != nil && !errors.Is(err, domain.ErrNotFound) {
					return out, err
				}
				if err == nil {
					base, previous = &candidate, &old
				}
			}
		}
		entry, err := domain.ProjectConversationSegment(ctx, snapshot, index, snapshotBindings, base, previous)
		if err != nil {
			return out, err
		}
		page.Entries = append(page.Entries, entry)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		return out, err
	}
	page.PageHash = domain.HashContent(raw)
	out.Segments = &page
	snapshots := out.Snapshots[in.SegmentOffset:end]
	out.Snapshots = nil
	if snapshots != nil {
		out.Snapshots = make([]domain.Snapshot, len(snapshots))
		for position, snapshot := range snapshots {
			out.Snapshots[position] = snapshot.Clone()
		}
	}
	out.Inclusion = nil
	out.History = []domain.HistoryEvent{}
	out.Semantics = domain.ContextSemantics{}
	return out, nil
}
