package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type ContextHistoryService struct {
	store   outbound.SessionStore
	history outbound.HistoryStore
}

func NewContextHistoryService(store outbound.SessionStore, history outbound.HistoryStore) *ContextHistoryService {
	return &ContextHistoryService{store: store, history: history}
}

func (s *ContextHistoryService) ValidateHistorySource(ctx context.Context, e domain.HistoryEvent) (domain.HistoryEvent, error) {
	if err := domain.ValidateHistoryEvent(e); err != nil {
		return e, err
	}
	for _, id := range []domain.ContentHash{e.Source, e.Target, e.SharedTarget, e.MemorySource} {
		if id == "" {
			continue
		}
		snap, err := s.store.GetSnapshot(ctx, id)
		if err != nil {
			return e, fmt.Errorf("source snapshot %s: %w", id, err)
		}
		if snap.RepoID != e.RepoID {
			return e, domain.ErrHashMismatch
		}
		doc, err := s.store.GetDoc(ctx, snap.DocHash)
		if err != nil {
			return e, err
		}
		if err = domain.ValidateSessionDocHash(doc); err != nil {
			return e, err
		}
	}
	if e.MemoryHash == "" && !e.MemoryPinned {
		from := e.Source
		if e.MemorySource != "" {
			from = e.MemorySource
		}
		memory, owner, err := s.inheritedMemory(ctx, e.RepoID, from)
		if err != nil {
			return e, err
		}
		e.MemoryHash = memory
		if owner != "" && owner != e.Source && owner != e.Target {
			e.MemorySource = owner
		}
	}
	if e.MemoryHash != "" {
		memory, err := s.store.GetMemory(ctx, e.MemoryHash)
		if err != nil {
			return e, err
		}
		if memory.SnapshotID != e.Source && memory.SnapshotID != e.Target && memory.SnapshotID != e.MemorySource {
			return e, domain.ErrHashMismatch
		}
	}
	e.MemoryPinned = true
	return e, nil
}

// Resolve project memory from verified ancestry only. Keep the owning snapshot
// as explicit provenance, rather than pretending its digest belongs to the
// descendant or importing its conversation into an orphan branch.
func (s *ContextHistoryService) inheritedMemory(ctx context.Context, repo string, from domain.ContentHash) (domain.ContentHash, domain.ContentHash, error) {
	seen := map[domain.ContentHash]bool{}
	queue := []domain.ContentHash{from}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		snap, err := s.store.GetSnapshot(ctx, id)
		if err != nil {
			return "", "", err
		}
		if snap.RepoID != repo {
			return "", "", domain.ErrHashMismatch
		}
		doc, err := s.store.GetDoc(ctx, snap.DocHash)
		if err != nil {
			return "", "", err
		}
		if err := domain.ValidateSessionDocHash(doc); err != nil {
			return "", "", err
		}
		if snap.MemoryHash != "" {
			return snap.MemoryHash, snap.ID, nil
		}
		queue = append(queue, snap.ReachabilityParents()...)
	}
	return "", "", nil
}

func (s *ContextHistoryService) RecordHistory(ctx context.Context, e domain.HistoryEvent) error {
	if err := domain.ValidateHistoryEvent(e); err != nil {
		return err
	}
	if domain.IsBranchBindingEvent(e) {
		events, err := s.history.ListHistoryEvents(ctx, e.RepoID)
		if err != nil {
			return err
		}
		if _, err := domain.ProjectContextBranches(append(events, e)); err != nil {
			return err
		}
	}
	return s.history.PutHistoryEvent(ctx, e)
}
func (s *ContextHistoryService) ListHistory(ctx context.Context, repoID string) ([]domain.HistoryEvent, error) {
	return s.history.ListHistoryEvents(ctx, repoID)
}

func (s *ContextHistoryService) SelectPosition(ctx context.Context, p domain.WorkingPosition) error {
	positions, ok := s.store.(outbound.WorkingPositionStore)
	if !ok {
		return fmt.Errorf("working position store unavailable")
	}
	var err error
	p, err = s.preparePosition(ctx, p, nil)
	if err != nil {
		return err
	}
	old, err := positions.GetWorkingPosition(ctx)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if old.RepoID != "" && old.RepoID != p.RepoID && (old.Snapshot != "" || old.MemoryHash != "" || old.MemorySource != "" || old.Selection != nil) {
		return domain.ErrHashMismatch
	}
	if err == nil && old.RepoID == p.RepoID && old.Branch == p.Branch && old.GitBranch() == p.GitBranch() && old.GitCommit == p.GitCommit && old.Snapshot == p.Snapshot && old.Orphan == p.Orphan {
		return nil // Do not repin mutable memory or create a second selection on replay.
	}
	p, err = positionSelection(old, p)
	if err != nil {
		return err
	}
	return positions.PutWorkingPosition(ctx, p)
}

// preparePosition shares content/memory validation with conditional selection.
// A supplied ref is the caller's observation, never a fresh implicit baseline.
func (s *ContextHistoryService) preparePosition(ctx context.Context, p domain.WorkingPosition, observed *domain.Ref) (domain.WorkingPosition, error) {
	events, err := s.history.ListHistoryEvents(ctx, p.RepoID)
	if err != nil {
		return p, err
	}
	branches, err := domain.ProjectContextBranches(events)
	if err != nil {
		return p, err
	}
	p.BranchID = branches.Identity(p.RepoID, p.Branch)
	verified, err := s.ValidateHistorySource(ctx, domain.HistoryEvent{ID: "00000000000000000000000000000000", RepoID: p.RepoID, BranchID: p.BranchID, Branch: p.Branch, Kind: "position", Source: p.Snapshot, MemorySource: p.MemorySource, MemoryHash: p.MemoryHash, MemoryPinned: p.MemoryPinned, GitAfter: p.GitCommit, CreatedAt: time.Now().UTC()})
	if err != nil {
		return p, err
	}
	p.MemoryHash, p.MemorySource = verified.MemoryHash, verified.MemorySource
	if observed != nil {
		p.SharedTarget = observed.Target
	} else {
		if ref, err := s.store.GetRef(ctx, p.RepoID, domain.RefBranch, p.Branch); err == nil {
			p.SharedTarget = ref.Target
		} else if !errors.Is(err, domain.ErrNotFound) && p.Branch != "" {
			return p, err
		}
	}
	forward, err := historyContains(ctx, s.store, p.Snapshot, p.SharedTarget)
	if err != nil {
		return p, err
	}
	p.MemoryPinned = true
	p.Rewound = p.Orphan || p.Branch == "" || !forward
	return p, nil
}

func positionSelection(old, p domain.WorkingPosition) (domain.WorkingPosition, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return p, err
	}
	e := domain.HistoryEvent{ID: hex.EncodeToString(id[:]), RepoID: p.RepoID, BranchID: p.BranchID, Branch: p.Branch, Kind: "position", MemoryPinned: true, Source: old.Snapshot, Target: p.Snapshot, MemoryHash: p.MemoryHash, MemorySource: p.MemorySource, GitBefore: old.GitCommit, GitAfter: p.GitCommit, CreatedAt: time.Now().UTC()}
	e.LocalBranch = p.LocalBranch
	if old.RepoID != p.RepoID {
		e.Source = ""
		e.GitBefore = ""
	}
	p.Selection = &e
	return p, nil
}

func historyContains(ctx context.Context, store outbound.SessionStore, from, ancestor domain.ContentHash) (bool, error) {
	if ancestor == "" {
		return true, nil
	}
	seen := map[domain.ContentHash]bool{}
	queue := []domain.ContentHash{from}
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if id == ancestor {
			return true, nil
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		snap, err := store.GetSnapshot(ctx, id)
		if err != nil {
			return false, err
		}
		queue = append(queue, snap.ReachabilityParents()...)
	}
	return false, nil
}

func (s *ContextHistoryService) CurrentPosition(ctx context.Context) (domain.WorkingPosition, error) {
	if store, ok := s.store.(outbound.WorkingPositionStore); ok {
		return store.GetWorkingPosition(ctx)
	}
	return domain.WorkingPosition{}, domain.ErrNotFound
}

func (s *ContextHistoryService) ResolveLocalBranch(ctx context.Context, repo, branch string) (domain.LocalBranchBinding, error) {
	if bindings, ok := s.store.(outbound.LocalBranchStore); ok {
		return bindings.ResolveLocalBranch(ctx, repo, branch)
	}
	return domain.LocalBranchBinding{LocalBranch: branch, Branch: branch, BranchID: domain.LegacyContextBranchID(repo, branch)}, nil
}

func (s *ContextHistoryService) BindLocalBranch(ctx context.Context, e domain.HistoryEvent) error {
	if bindings, ok := s.store.(outbound.LocalBranchStore); ok {
		if e.Kind == "attach" && e.SharedTarget != "" {
			ref, err := s.store.GetRef(ctx, e.RepoID, domain.RefBranch, e.Branch)
			if errors.Is(err, domain.ErrNotFound) {
				_, err = s.store.CreateBranchRef(ctx, domain.Ref{Kind: domain.RefBranch, RepoID: e.RepoID, Name: e.Branch, Target: e.SharedTarget})
				if err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else if ref.Target != e.SharedTarget {
				forward, err := historyContains(ctx, s.store, e.SharedTarget, ref.Target)
				if err != nil {
					return err
				}
				if forward {
					ref.Target = e.SharedTarget
					if err := s.store.PutRef(ctx, ref); err != nil {
						return err
					}
				}
			}
		}
		return bindings.BindLocalBranch(ctx, e)
	}
	return fmt.Errorf("local branch binding store unavailable")
}

// A selected historical position pins the memory object, including an empty
// memory. Following the snapshot's current mutable pointer would import later
// work into an earlier code position.
func selectedMemory(ctx context.Context, store MemoryReader, id domain.ContentHash) (domain.MemoryDigest, bool, error) {
	positions, ok := store.(outbound.WorkingPositionReader)
	if !ok {
		return domain.MemoryDigest{}, false, nil
	}
	p, err := positions.GetWorkingPosition(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.MemoryDigest{}, false, nil
	}
	if err != nil {
		return domain.MemoryDigest{}, false, err
	}
	if p.Snapshot != id || !p.Rewound {
		return domain.MemoryDigest{}, false, nil
	}
	if p.MemoryHash == "" {
		return domain.MemoryDigest{}, true, nil
	}
	memory, err := store.GetMemory(ctx, p.MemoryHash)
	return memory, true, err
}
