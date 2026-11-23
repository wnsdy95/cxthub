package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *FSStore) historyPath(repoID domain.ContentHash, id string) string {
	return filepath.Join(s.repoDir(repoID), "history", id+".json")
}
func (s *FSStore) historyJournal(repoID domain.ContentHash) string {
	return filepath.Join(s.repoDir(repoID), ".history-txn.json")
}

func (s *FSStore) validateHistoryProjection(ctx context.Context, e domain.HistoryEvent, replay bool) error {
	if err := domain.ValidateHistoryEvent(e); err != nil {
		return err
	}
	repoID := domain.ContentHash(e.RepoID)
	if err := s.requireSnapshots(ctx, repoID, e.Source, e.Target, e.SharedTarget, e.MemorySource); err != nil {
		return err
	}
	for _, root := range historyRoots(e) {
		ref, err := s.getRefRaw(ctx, repoID, root.Kind, root.Name)
		if err != nil && err != domain.ErrNotFound {
			return err
		}
		if err == nil && ref.Target != root.Target {
			return domain.ErrIntegrity
		}
	}
	if e.Kind == "advance" {
		ref, err := s.GetRef(ctx, repoID, domain.RefBranch, e.Branch)
		if err != nil {
			return err
		}
		if ref.Target != e.Source && !(replay && ref.Target == e.Target) {
			return domain.ErrRefConflict
		}
	}
	if e.Kind == "rename" || e.Kind == "archive" {
		name := e.Branch
		if e.Kind == "rename" {
			name = e.PreviousBranch
		}
		ref, err := s.GetRef(ctx, repoID, domain.RefBranch, name)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if err == nil && ref.Target != e.Source {
			return domain.ErrRefConflict
		}
	}
	return nil
}

// Retention and the operation record precede the current ref. A durable redo
// journal completes an interrupted publication before the next graph mutation.
func (s *FSStore) recoverHistoryEvent(ctx context.Context, repoID domain.ContentHash) error {
	raw, err := os.ReadFile(s.historyJournal(repoID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var e domain.HistoryEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return err
	}
	if domain.ContentHash(e.RepoID) != repoID {
		return domain.ErrIntegrity
	}
	if err := s.validateHistoryProjection(ctx, e, true); err != nil {
		return err
	}
	if old, err := os.ReadFile(s.historyPath(repoID, e.ID)); err == nil {
		var previous domain.HistoryEvent
		if json.Unmarshal(old, &previous) != nil || !reflect.DeepEqual(previous, e) {
			return domain.ErrIntegrity
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, root := range historyRoots(e) {
		if err := writeAtomic(s.refFile(repoID, root.Kind, root.Name), []byte(string(root.Target)+"\n")); err != nil {
			return err
		}
	}
	if err := writeAtomic(s.historyPath(repoID, e.ID), raw); err != nil {
		return err
	}
	if e.Kind == "advance" {
		current, err := s.GetRef(ctx, repoID, domain.RefBranch, e.Branch)
		if err != nil {
			return err
		}
		if current.Target != e.Target {
			if err := writeAtomic(s.refFile(repoID, domain.RefBranch, e.Branch), encodeRef(domain.Ref{RepoID: repoID, Kind: domain.RefBranch, Name: e.Branch, BranchID: e.BranchID, Target: e.Target})); err != nil {
				return err
			}
			s.appendReflog(repoID, domain.RefLogEntry{Kind: domain.RefBranch, Name: e.Branch, Old: e.Source, New: e.Target, CreatedAt: time.Now().UTC()})
		}
	}
	if err := s.applyProtocolHistory(ctx, e); err != nil {
		return err
	}
	return removeFileDurable(s.historyJournal(repoID))
}
