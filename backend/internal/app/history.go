package app

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *Service) ListHistory(ctx context.Context, repoID domain.ContentHash) ([]domain.HistoryEvent, error) {
	store, ok := s.meta.(outbound.HistoryStore)
	if !ok {
		return nil, fmt.Errorf("context history storage unavailable")
	}
	return store.ListHistoryEvents(ctx, repoID)
}

func (s *Service) RecordHistory(ctx context.Context, event domain.HistoryEvent) error {
	return s.recordHistoryWithDocumentPreparation(ctx, event)
}

// A verification set lives for one operation, not on the service or store.
// Keys include repository ownership and both immutable snapshot/document IDs.
type historyDocumentKey struct {
	repo, snapshot domain.ContentHash
	document       domain.DocumentRef
}
type historyVerification map[historyDocumentKey]struct{}

func (s *Service) recordHistory(ctx context.Context, event domain.HistoryEvent, serverReceipt bool, verified historyVerification) error {
	return s.recordHistoryPrepared(ctx, event, serverReceipt, verified, nil)
}

func (s *Service) recordHistoryPrepared(ctx context.Context, event domain.HistoryEvent, serverReceipt bool, verified historyVerification, prepare func(context.Context, []domain.ContentHash) (historyVerification, error)) error {
	if err := domain.ValidateHistoryEvent(event); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrValidation, err)
	}
	repoID := domain.ContentHash(event.RepoID)
	store, ok := s.meta.(outbound.HistoryStore)
	if !ok {
		return fmt.Errorf("context history storage unavailable")
	}
	// An acknowledged immutable event remains acknowledged if policy or the
	// branch changes later. A retry must never reapply the old ref movement.
	accepted, err := store.ListHistoryEvents(ctx, repoID)
	if err != nil {
		return err
	}
	for _, old := range accepted {
		if old.ID != event.ID {
			continue
		}
		if !reflect.DeepEqual(old, event) {
			return domain.ErrRefConflict
		}
		return s.wakePublishedPRJobs(ctx, event)
	}
	var predecessor domain.HistoryEvent
	if event.MemorySelectionParent != "" {
		// The predecessor is immutable accepted evidence from this repository's
		// current write transaction, never a caller-supplied or mutable position.
		for _, old := range accepted {
			if old.ID == event.MemorySelectionParent {
				predecessor = old
				break
			}
		}
		if !domain.IsInitialMemorySelection(predecessor, event) {
			return fmt.Errorf("%w: memory selection requires an accepted exact empty or inherited predecessor", domain.ErrConflict)
		}
	}
	if err := domain.ValidateCreationOrigin(accepted, event); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrConflict, err)
	}
	if event.Kind == "pr-merge" && !serverReceipt {
		return fmt.Errorf("%w: PR bindings are issued by PR promotion", domain.ErrForbidden)
	}
	if event.Kind == "publish" {
		proven := false
		for _, old := range accepted {
			if domain.IsPublicationProof(event, old) {
				proven = true
				break
			}
		}
		if !proven {
			return fmt.Errorf("%w: publication requires an accepted exact source observation", domain.ErrConflict)
		}
	}
	repo, err := s.meta.GetRepo(ctx, repoID)
	if err != nil {
		return err
	}
	if verified == nil {
		verified = make(historyVerification)
	}
	roots := []domain.ContentHash{event.Source, event.Target, event.SharedTarget, event.MemorySource}
	if predecessor.MemoryHash != "" {
		roots = append(roots, predecessor.Source, predecessor.Target, predecessor.SharedTarget, predecessor.MemorySource)
	}
	if prepare != nil {
		verified, err = prepare(ctx, roots)
		if err != nil {
			return err
		}
	}
	for _, id := range roots {
		if id == "" {
			continue
		}
		snap, err := s.meta.GetSnapshot(ctx, repoID, id)
		if err != nil {
			return err
		}
		key := historyDocumentKey{repo: repoID, snapshot: snap.ID, document: snap.DocumentRef()}
		if _, ok := verified[key]; ok {
			continue
		}
		if err := s.verifyStoredSnapshotDoc(ctx, repoID, snap); err != nil {
			return err
		}
		verified[key] = struct{}{}
	}
	if event.MemoryHash != "" {
		memory, err := s.blobs.GetMemory(ctx, repoID, event.MemoryHash)
		if err != nil {
			return err
		}
		if memory.SnapshotID != event.Source && memory.SnapshotID != event.Target && memory.SnapshotID != event.MemorySource {
			return domain.ErrIntegrity
		}
		if event.MemorySelectionParent != "" {
			hash, err := domain.MemoryDigestHash(memory)
			if err != nil || hash != event.MemoryHash || memory.SnapshotID != event.Target || memory.PreviousMemoryHash != "" {
				return domain.ErrIntegrity
			}
			// An accepted inherited pin is evidence only while its immutable
			// chain verifies. Keep every read in this repository write transaction.
			seen := map[domain.ContentHash]bool{}
			for id := predecessor.MemoryHash; id != ""; {
				if err := ctx.Err(); err != nil {
					return err
				}
				if domain.ValidateContentHash(id) != nil || seen[id] {
					return domain.ErrIntegrity
				}
				seen[id] = true
				prior, err := s.blobs.GetMemory(ctx, repoID, id)
				if err != nil {
					return err
				}
				actual, err := domain.MemoryDigestHash(prior)
				if err != nil || actual != id || prior.SnapshotID != predecessor.MemorySource ||
					domain.ValidateOptionalContentHash(prior.PreviousMemoryHash) != nil {
					return domain.ErrIntegrity
				}
				id = prior.PreviousMemoryHash
			}
		}
	}
	protectedName := event.Branch
	if event.Kind == "rename" {
		protectedName = event.PreviousBranch
	}
	if (event.Kind == "advance" || event.Kind == "rename" || event.Kind == "archive") && repo.ProtectDefault && protectedName == repo.DefaultBranch {
		return fmt.Errorf("%w: %s cannot modify protected branch %q", domain.ErrForbidden, event.Kind, protectedName)
	}
	if err := store.ApplyHistoryEvent(ctx, event); err != nil {
		return err
	}
	if event.Kind == "advance" {
		if err := s.notifyRefUpdate(ctx, repoID, domain.Ref{RepoID: repoID, Kind: domain.RefBranch, Name: event.Branch, Target: event.Target}, true, false); err != nil {
			return err
		}
	}
	return s.wakePublishedPRJobs(ctx, event)
}

func (s *Service) wakePublishedPRJobs(ctx context.Context, event domain.HistoryEvent) error {
	if err := s.queueHistoryGitScan(ctx, event); err != nil {
		return err
	}
	if event.Kind == "publish" {
		if jobs, ok := s.meta.(outbound.PRJobStore); ok {
			return jobs.WakePRSourceJobs(ctx, domain.ContentHash(event.RepoID), time.Now().UTC())
		}
	}
	return nil
}
