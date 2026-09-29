package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// finishContextQuery adds bounded, opt-in range metadata to the already selected
// server view. Inclusion and ordering stay owned by QueryContext/branchContext.
func (s *Service) finishContextQuery(ctx context.Context, repo domain.ContentHash, in domain.ContextSelection, out domain.ContextQueryView, history []domain.HistoryEvent, key contextSegmentCacheKey) (domain.ContextQueryView, error) {
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
	if in.SegmentLimit == 0 {
		return out, nil
	}
	if in.SegmentOffset > len(out.Snapshots) {
		return out, fmt.Errorf("%w: segment offset exceeds selection", domain.ErrValidation)
	}
	bindings := contextPublicationBindings(repo, history)
	type sourceBindings struct {
		SnapshotID domain.ContentHash
		Bindings   []domain.CommitContextBinding
	}
	receipts := make([]sourceBindings, 0, len(out.Snapshots))
	for _, snap := range out.Snapshots {
		receipts = append(receipts, sourceBindings{snap.ID, bindings[snap.ID]})
	}
	// Pin actual selection dependencies, not unrelated pending traffic elsewhere
	// in the repository. Immutable document hashes fix index/range inputs; exact
	// finalized bindings and selected metadata invalidate a changed projection.
	basis, err := json.Marshal(struct {
		Version      int
		Selection    domain.ContextSelection
		Position     domain.ContentHash
		Inclusion    *domain.BranchContext
		Snapshots    []domain.Snapshot
		Publications []sourceBindings
	}{domain.ContextSegmentVersion, domain.ContextSelection{Branch: out.Branch, Scope: in.Scope, CodeCommit: in.CodeCommit}, out.Position, out.Inclusion, out.Snapshots, receipts})
	if err != nil {
		return out, err
	}
	state := domain.HashContent(basis)
	if in.SegmentStateHash != "" && in.SegmentStateHash != state {
		return out, fmt.Errorf("%w: context segments changed; restart without offset", domain.ErrConflict)
	}
	out.StateHash = state
	selected := make(map[domain.ContentHash]domain.Snapshot, len(out.Snapshots))
	for _, snap := range out.Snapshots {
		selected[snap.ID] = snap
	}
	// Immutable documents are shared only inside this authorized repository query.
	basisView := contextSegmentBasis{view: out, selected: selected, bindings: bindings}
	if key.revision.Graph != 0 || key.revision.Evidence != 0 {
		s.segmentCache.put(key, basisView)
		resolved := key
		resolved.position, resolved.branch = string(out.Position), out.Branch
		s.segmentCache.put(resolved, basisView)
	}
	return s.contextSegmentPage(ctx, repo, in, basisView)
}

func (s *Service) contextSegmentPage(ctx context.Context, repo domain.ContentHash, in domain.ContextSelection, basis contextSegmentBasis) (domain.ContextQueryView, error) {
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
		if idx, ok := indexes[hash]; ok {
			return idx, nil
		}
		idx, err := s.docReadIndex(ctx, repo, hash)
		if err == nil {
			// Keep at most the source and adjacent baseline metadata alive between rows.
			// A page of many large captures must not retain every complete event index.
			if len(indexOrder) == 2 {
				delete(indexes, indexOrder[0])
				indexOrder = indexOrder[1:]
			}
			indexes[hash] = idx
			indexOrder = append(indexOrder, hash)
		}
		return idx, err
	}
	end := min(in.SegmentOffset+in.SegmentLimit, len(out.Snapshots))
	page := domain.ContextSegmentPage{Version: domain.ContextSegmentVersion, StateHash: state, Offset: in.SegmentOffset, Next: -1, Total: len(out.Snapshots), Complete: end == len(out.Snapshots), Entries: []domain.ContextSegmentCoverage{}}
	if !page.Complete {
		page.Next = end
	}
	for _, snap := range out.Snapshots[in.SegmentOffset:end] {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if snap.RepoID != repo {
			return out, domain.ErrIntegrity
		}
		idx, err := read(snap.DocHash)
		if errors.Is(err, domain.ErrNotFound) {
			page.Entries = append(page.Entries, domain.ContextSegmentCoverage{SnapshotID: snap.ID, Kind: "unavailable", Reason: "source_unavailable", TotalEvents: -1, Bindings: append([]domain.CommitContextBinding{}, bindings[snap.ID]...)})
			continue
		}
		if err != nil {
			return out, err
		}
		var base *domain.Snapshot
		var previous *domain.DocReadIndex
		// Multiple natural parents are ambiguous as a capture baseline. Grafts never
		// prove native-stream containment, even if their text happens to match.
		if len(bindings[snap.ID]) > 0 && len(snap.Parents) == 1 && snap.Parents[0] != snap.ID {
			if candidate, ok := selected[snap.Parents[0]]; ok && len(bindings[candidate.ID]) > 0 {
				old, err := read(candidate.DocHash)
				if err != nil && !errors.Is(err, domain.ErrNotFound) {
					return out, err
				}
				if err == nil {
					base, previous = &candidate, &old
				}
			}
		}
		entry, err := domain.ProjectConversationSegment(ctx, snap, idx, bindings[snap.ID], base, previous)
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
	// A segment page carries only its own snapshot metadata. Full inclusion,
	// graph semantics and history remain in the unpaged context-query response.
	out.Snapshots = out.Snapshots[in.SegmentOffset:end]
	out.Inclusion = nil
	out.History = []domain.HistoryEvent{}
	out.Semantics = domain.ContextSemantics{}
	return out, nil
}

// Matching by the exact accepted observation key avoids quadratic history scans.
// A raw position/birth event, snapshot label, or inferred Git timestamp is never
// promoted to a finalized commit binding by this read projection.
func contextPublicationBindings(repo domain.ContentHash, history []domain.HistoryEvent) map[domain.ContentHash][]domain.CommitContextBinding {
	type proofKey struct {
		repo, branchID, branch, local, worktree, git string
		target                                       domain.ContentHash
	}
	key := func(e domain.HistoryEvent) proofKey {
		return proofKey{e.RepoID, e.BranchID, e.Branch, e.LocalBranch, e.WorktreeID, e.GitAfter, e.Target}
	}
	observed := map[proofKey]domain.HistoryEvent{}
	for _, e := range history {
		if e.RepoID == string(repo) && e.Kind != "publish" && e.Kind != "pr-merge" && domain.ValidateHistoryEvent(e) == nil {
			observed[key(e)] = e
		}
	}
	out := map[domain.ContentHash][]domain.CommitContextBinding{}
	for _, e := range history {
		if e.RepoID != string(repo) || e.Kind != "publish" || domain.ValidateHistoryEvent(e) != nil || !domain.IsPublicationProof(e, observed[key(e)]) {
			continue
		}
		out[e.Target] = append(out[e.Target], domain.CommitContextBinding{EventID: e.ID, GitCommit: e.GitAfter, BranchID: e.BranchID, WorktreeID: e.WorktreeID, SegmentIDs: []domain.ContentHash{}})
	}
	for _, entries := range out {
		sort.Slice(entries, func(i, j int) bool { return entries[i].EventID < entries[j].EventID })
	}
	return out
}
