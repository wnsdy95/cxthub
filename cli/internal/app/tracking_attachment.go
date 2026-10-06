package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func (s *ContextHistoryService) PrepareTrackingAttachment(ctx context.Context, e domain.HistoryEvent, observed inbound.RemoteBranchObservation, ancestry []string) (domain.TrackingAttachment, error) {
	var zero domain.TrackingAttachment
	ref := observed.Ref
	if ref.RepoID != e.RepoID {
		return zero, domain.ErrHashMismatch
	}
	local, err := s.history.ListHistoryEvents(ctx, e.RepoID)
	if err != nil {
		return zero, err
	}
	owners, err := s.trackingMemoryOwners(ctx, ref, observed.History, local)
	if err != nil {
		return zero, err
	}
	history, err := domain.TrackingHistoryWithMemoryOwners(ref, observed.History, local, owners)
	if err != nil {
		return zero, err
	}
	identity := ref.BranchID
	if identity == "" {
		identity = domain.LegacyContextBranchID(e.RepoID, ref.Name)
	}
	graph := make(map[domain.ContentHash]domain.Snapshot, len(observed.Snapshots))
	for _, snap := range observed.Snapshots {
		if snap.RepoID != e.RepoID {
			return zero, domain.ErrHashMismatch
		}
		graph[snap.ID] = snap
	}
	reachable := map[domain.ContentHash]map[domain.ContentHash]bool{}
	contains := func(from, ancestor domain.ContentHash) (bool, error) {
		if seen, ok := reachable[from]; ok {
			return seen[ancestor], nil
		}
		seen := map[domain.ContentHash]bool{}
		queue := []domain.ContentHash{from}
		for len(queue) > 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			id := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if seen[id] {
				continue
			}
			seen[id] = true
			if snap, ok := graph[id]; ok {
				queue = append(queue, snap.ReachabilityParents()...)
			}
		}
		reachable[from] = seen
		return seen[ancestor], nil
	}
	byCode := map[string][]domain.HistoryEvent{}
	for _, event := range history {
		if event.BranchID != identity || event.Kind == "publish" {
			continue
		}
		code := event.GitAfter
		if event.Kind == "pr-merge" {
			if !event.PRCompleted || event.PR == nil {
				continue
			}
			code = event.PR.MergeSHA
		}
		if code != "" {
			byCode[code] = append(byCode[code], event)
		}
	}
	if len(ancestry) == 0 || ancestry[0] != e.GitAfter {
		return zero, domain.ErrCodePositionMismatch
	}
	var selected domain.WorkingPosition
	selectedCode := ""
	for _, code := range ancestry {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if !domain.ValidGitOID(code) {
			return zero, domain.ErrHashMismatch
		}
		byTarget := map[domain.ContentHash][]domain.WorkingPosition{}
		events, err := s.initialMemorySelections(ctx, byCode[code])
		if err != nil {
			return zero, err
		}
		for _, event := range events {
			if event.Kind == "pr-merge" {
				p, err := s.ResolvePRSourcePositionFromHistory(ctx, event, history)
				if err != nil {
					return zero, err
				}
				byTarget[p.Snapshot] = append(byTarget[p.Snapshot], p)
			} else if domain.IsPinnedContextEvent(event) {
				if _, err := s.ValidateHistorySource(ctx, event); err != nil {
					return zero, err
				}
				p := domain.WorkingPosition{Snapshot: event.Target, MemoryHash: event.MemoryHash, MemorySource: event.MemorySource, MemoryPinned: true}
				byTarget[p.Snapshot] = append(byTarget[p.Snapshot], p)
			}
		}
		if len(byTarget) == 0 {
			continue
		}
		for target, candidates := range byTarget {
			dominates := true
			for other := range byTarget {
				included, err := contains(target, other)
				if err != nil {
					return zero, err
				}
				if !included {
					dominates = false
					break
				}
			}
			if !dominates {
				continue
			}
			if selected.Snapshot != "" && selected.Snapshot != target {
				return zero, domain.ErrSyncConflict
			}
			selected, err = s.recordedMemorySelection(ctx, candidates)
			if err != nil {
				return zero, err
			}
		}
		if selected.Snapshot == "" {
			return zero, fmt.Errorf("%w: tracking code has conflicting or incomplete observed graph evidence", domain.ErrSyncConflict)
		}
		selectedCode = code
		break
	}
	if selected.Snapshot == "" {
		return zero, fmt.Errorf("%w: tracking context has no pinned association on Git %s ancestry; operation remains queued", domain.ErrNotFound, e.GitAfter)
	}
	originalBranch := e.Branch
	e.Kind, e.Branch, e.BranchID, e.LocalBranch = "attach", ref.Name, identity, originalBranch
	e.Source, e.Target, e.SharedTarget = selected.Snapshot, selected.Snapshot, ref.Target
	e.MemoryHash, e.MemorySource, e.MemoryPinned = selected.MemoryHash, selected.MemorySource, true
	e.BindingParent = ""
	if e.Creation != nil && e.Creation.Evidence == "process-argv" && e.Creation.OriginBranch == ref.Name {
		copy := *e.Creation
		copy.OriginBranchID = identity
		e.Creation = &copy
	}
	e, err = s.ValidateHistorySource(ctx, e)
	if err != nil {
		return zero, err
	}
	proof, err := domain.TrackingProof(ref, history, selectedCode, selected.Snapshot)
	if err != nil {
		return zero, err
	}
	forward, err := contains(selected.Snapshot, ref.Target)
	if err != nil {
		return zero, err
	}
	a := domain.TrackingAttachment{Event: e, ObservedRef: ref, Proof: proof, Code: selectedCode, Rewound: !forward}
	return a, domain.ValidateTrackingAttachmentCompatibilityWithMemoryOwners(local, a, owners)
}

// Ownership witnesses live only for this preparation. Never infer them from
// an optional source field or the snapshot's mutable memory attachment.
func (s *ContextHistoryService) trackingMemoryOwners(ctx context.Context, ref domain.Ref, groups ...[]domain.HistoryEvent) (map[domain.ContentHash]domain.ContentHash, error) {
	owners := map[domain.ContentHash]domain.ContentHash{}
	identity := ref.BranchID
	if identity == "" {
		identity = domain.LegacyContextBranchID(ref.RepoID, ref.Name)
	}
	for _, events := range groups {
		for _, e := range events {
			if e.BranchID != identity || !e.MemoryPinned || e.MemoryHash == "" || e.Kind == "publish" || e.Kind == "pr-merge" || owners[e.MemoryHash] != "" {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			memory, err := s.store.GetMemory(ctx, e.MemoryHash)
			if err != nil {
				return nil, err
			}
			hash, err := domain.MemoryDigestHash(memory)
			if err != nil || hash != e.MemoryHash || domain.ValidateContentHash(memory.SnapshotID) != nil {
				return nil, domain.ErrHashMismatch
			}
			owners[e.MemoryHash] = memory.SnapshotID
		}
	}
	return owners, nil
}

func (s *ContextHistoryService) ApplyTrackingAttachment(ctx context.Context, in inbound.TrackingAttachmentInput) error {
	store, ok := s.store.(outbound.TrackingAttachmentStore)
	if !ok {
		return fmt.Errorf("durable tracking attachment store unavailable")
	}
	a := in.Attachment
	if err := domain.ValidateTrackingAttachment(a); err != nil {
		return err
	}
	// The store checks current object bytes together with the final CAS. Doing
	// this again here would duplicate reads and still leave a mutation gap.
	change := outbound.TrackingAttachmentCommit{Attachment: a, ExpectedRef: in.ExpectedRef, RequirePristine: in.RequirePristine, ObservedSnapshots: in.ObservedSnapshots}
	if in.SelectPosition {
		e := a.Event
		p := domain.WorkingPosition{RepoID: e.RepoID, Branch: e.Branch, BranchID: e.BranchID, LocalBranch: e.LocalBranch, WorktreeID: e.WorktreeID, GitCommit: e.GitAfter, Snapshot: e.Target, SharedTarget: e.SharedTarget, MemoryHash: e.MemoryHash, MemorySource: e.MemorySource, MemoryPinned: true}
		if p.LocalBranch == p.Branch {
			p.LocalBranch = ""
		}
		p.Rewound = a.Rewound
		old := domain.WorkingPosition{}
		if in.ExpectedPosition != nil {
			old = *in.ExpectedPosition
		}
		p, err := positionSelection(old, p)
		if err != nil {
			return err
		}
		change.Position = &outbound.TrackingPositionCAS{Expected: in.ExpectedPosition, Next: p}
	}
	return store.CommitTrackingAttachment(ctx, change)
}

func (s *ContextHistoryService) TrackingPristine(ctx context.Context, repo string) (bool, error) {
	store, ok := s.store.(outbound.PristineTrackingStore)
	if !ok {
		return false, fmt.Errorf("pristine tracking store unavailable")
	}
	return store.TrackingPristine(ctx, repo)
}

func (s *ContextHistoryService) InitializeCapturePosition(ctx context.Context, expected *domain.WorkingPosition, next domain.WorkingPosition) error {
	store, ok := s.store.(outbound.InitialCapturePositionStore)
	if !ok {
		return fmt.Errorf("safe initial capture position store unavailable")
	}
	return store.InitializeCapturePosition(ctx, expected, next)
}
