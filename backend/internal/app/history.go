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
	return repositoryWriteError(ctx, s, domain.ContentHash(event.RepoID), func(ctx context.Context) error { return s.recordHistory(ctx, event, false, nil) })
}

// A verification set lives for one operation, not on the service or store.
// Keys include repository ownership and both immutable snapshot/document IDs.
type historyVerification map[[3]domain.ContentHash]struct{}

func (s *Service) recordHistory(ctx context.Context, event domain.HistoryEvent, serverReceipt bool, verified historyVerification) error {
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
	for _, id := range []domain.ContentHash{event.Source, event.Target, event.SharedTarget, event.MemorySource} {
		if id == "" {
			continue
		}
		snap, err := s.meta.GetSnapshot(ctx, repoID, id)
		if err != nil {
			return err
		}
		key := [3]domain.ContentHash{repoID, snap.ID, snap.DocHash}
		if _, ok := verified[key]; ok {
			continue
		}
		doc, err := s.blobs.GetDoc(ctx, repoID, snap.DocHash)
		if err != nil {
			return err
		}
		if err = s.engine.VerifyIntegrity(ctx, snap, doc); err != nil {
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
