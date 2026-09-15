package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *FSStore) historyPath(repoID domain.ContentHash, id string) string {
	return filepath.Join(s.repoDir(repoID), "history", id+".json")
}
func (s *FSStore) historyJournal(repoID domain.ContentHash) string {
	return filepath.Join(s.repoDir(repoID), ".history-txn.json")
}

func (s *FSStore) ApplyHistoryEvent(ctx context.Context, e domain.HistoryEvent) error {
	if err := domain.ValidateHistoryEvent(e); err != nil {
		return err
	}
	repoID := domain.ContentHash(e.RepoID)
	lock := s.refLock(repoID, domain.RefBranch, e.Branch)
	lock.Lock()
	defer lock.Unlock()
	if err := s.recoverHistoryEvent(ctx, repoID); err != nil {
		return err
	}
	raw, err := os.ReadFile(s.historyPath(repoID, e.ID))
	if err == nil {
		var old domain.HistoryEvent
		if json.Unmarshal(raw, &old) != nil || !reflect.DeepEqual(old, e) {
			return domain.ErrRefConflict
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if e.Kind == "advance" || e.Kind == "rename" || e.Kind == "archive" {
		repo, err := s.GetRepo(ctx, repoID)
		if err != nil {
			return err
		}
		name := e.Branch
		if e.Kind == "rename" {
			name = e.PreviousBranch
		}
		if repo.ProtectDefault && repo.DefaultBranch == name {
			return domain.ErrForbidden
		}
	}
	if domain.IsBranchBindingEvent(e) || e.Kind == "advance" || e.Kind == "attach" {
		events, err := s.listHistoryEventsRaw(repoID)
		if err != nil {
			return err
		}
		if err := domain.ValidateHistoryBranch(events, e); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrRefConflict, err)
		}
	}
	if err := s.validateHistoryProjection(ctx, e, false); err != nil {
		return err
	}
	raw, err = json.Marshal(e)
	if err != nil {
		return err
	}
	if err := writeAtomic(s.historyJournal(repoID), raw); err != nil {
		return err
	}
	return s.recoverHistoryEvent(ctx, repoID)
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
			if err := writeAtomic(s.refFile(repoID, domain.RefBranch, e.Branch), []byte(string(e.Target)+"\n")); err != nil {
				return err
			}
			s.appendReflog(repoID, domain.RefLogEntry{Kind: domain.RefBranch, Name: e.Branch, Old: e.Source, New: e.Target, CreatedAt: time.Now().UTC()})
		}
	}
	return removeFileDurable(s.historyJournal(repoID))
}

func (s *FSStore) recoverHistoryJournals() error {
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "repos"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		repoID := domain.ContentHash("sha256:" + entry.Name())
		if err := s.recoverHistoryEvent(context.Background(), repoID); err != nil {
			return err
		}
	}
	return nil
}

func (s *FSStore) ListHistoryEvents(ctx context.Context, repoID domain.ContentHash) ([]domain.HistoryEvent, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	lock := s.refLock(repoID, domain.RefBranch, "")
	lock.Lock()
	defer lock.Unlock()
	if err := s.recoverHistoryEvent(ctx, repoID); err != nil {
		return nil, err
	}
	return s.listHistoryEventsRaw(repoID)
}

func (s *FSStore) listHistoryEventsRaw(repoID domain.ContentHash) ([]domain.HistoryEvent, error) {
	entries, err := os.ReadDir(filepath.Join(s.repoDir(repoID), "history"))
	out := []domain.HistoryEvent{}
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.repoDir(repoID), "history", entry.Name()))
		if err != nil {
			return nil, err
		}
		var e domain.HistoryEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, err
		}
		if err := domain.ValidateHistoryEvent(e); err != nil {
			return nil, err
		}
		if e.RepoID != string(repoID) || entry.Name() != e.ID+".json" {
			return nil, domain.ErrIntegrity
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}
