package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Ref mutations and history writes use the same cross-process lock. Selecting
// both together prevents a concurrent branch birth or commit from entering the
// final publication without the objects chosen for this push.
func (s *FileStore) ReadPushCatalog(ctx context.Context, repo string) (man domain.Manifest, events []domain.HistoryEvent, err error) {
	err = s.withRefMutationLock(ctx, func() error {
		var e error
		events, e = s.listHistoryEventsAndPositions(repo)
		if e != nil {
			return e
		}
		man, e = s.Manifest(ctx, repo)
		return e
	})
	return
}

func (s *FileStore) PutHistoryEvent(ctx context.Context, event domain.HistoryEvent) error {
	if err := domain.ValidateHistoryEvent(event); err != nil {
		return err
	}
	return s.withRefMutationLock(ctx, func() error { return s.putHistoryEvent(event) })
}

// Called under the repository ref lock. Immutable retention roots precede the
// event, so interrupted writes can only retain extra data.
func (s *FileStore) putHistoryEvent(event domain.HistoryEvent) error {
	if err := domain.ValidateHistoryEvent(event); err != nil {
		return err
	}
	path := filepath.Join(s.storeDir(), "history", event.ID+".json")
	raw, err := readCxtFile(path)
	if err == nil {
		var old domain.HistoryEvent
		if json.Unmarshal(raw, &old) != nil || !reflect.DeepEqual(old, event) {
			return fmt.Errorf("history event %s is immutable", event.ID)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if domain.IsBranchBindingEvent(event) {
		events, err := s.listHistoryEvents(event.RepoID)
		if err != nil {
			return err
		}
		if _, err := domain.ProjectContextBranches(append(events, event)); err != nil {
			return err
		}
	}
	if err := s.retainHistoryRoots(event); err != nil {
		return err
	}
	raw, err = json.Marshal(event)
	if err != nil {
		return err
	}
	return writeAtomic(path, raw)
}

func (s *FileStore) retainHistoryRoots(event domain.HistoryEvent) error {
	for kind, id := range map[string]domain.ContentHash{"shared-target": event.SharedTarget, "source": event.Source, "target": event.Target, "memory-source": event.MemorySource} {
		if id == "" {
			continue
		}
		name := "cxt/history/v1/" + event.ID + "/" + kind
		ref, err := s.getRefRaw(context.Background(), event.RepoID, domain.RefTag, name)
		if err == nil && ref.Target != id {
			return domain.ErrHashMismatch
		}
		if err != nil && err != domain.ErrNotFound {
			return err
		}
		if err := s.putRefRaw(domain.Ref{RepoID: event.RepoID, Kind: domain.RefTag, Name: name, Target: id}); err != nil {
			return err
		}
	}
	return nil
}

func (s *FileStore) listHistoryEvents(repoID string) ([]domain.HistoryEvent, error) {
	path := filepath.Join(s.storeDir(), "history")
	if err := validateCxtDir(path); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return []domain.HistoryEvent{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []domain.HistoryEvent{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		raw, err := readCxtFile(filepath.Join(path, entry.Name()))
		if err != nil {
			return nil, err
		}
		var e domain.HistoryEvent
		if err = json.Unmarshal(raw, &e); err != nil {
			return nil, err
		}
		if err = domain.ValidateHistoryEvent(e); err != nil {
			return nil, err
		}
		if entry.Name() != e.ID+".json" {
			return nil, domain.ErrHashMismatch
		}
		if repoID == "" || e.RepoID == repoID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *FileStore) listHistoryEventsAndPositions(repoID string) ([]domain.HistoryEvent, error) {
	var out []domain.HistoryEvent
	err := func() error {
		var err error
		out, err = s.listHistoryEvents(repoID)
		if err != nil {
			return err
		}
		dir := filepath.Join(s.storeDir(), "worktrees")
		if err := validateCxtDir(dir); err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		seen := map[string]domain.HistoryEvent{}
		for _, e := range out {
			seen[e.ID] = e
		}
		for _, entry := range entries {
			raw, err := readCxtFile(filepath.Join(dir, entry.Name(), "position.json"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			var p domain.WorkingPosition
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			if p.WorktreeID != entry.Name() {
				return domain.ErrHashMismatch
			}
			if p.Selection == nil || (repoID != "" && p.RepoID != repoID) {
				continue
			}
			e := *p.Selection
			if err := domain.ValidateHistoryEvent(e); err != nil {
				return err
			}
			if old, ok := seen[e.ID]; ok {
				if !reflect.DeepEqual(old, e) {
					return domain.ErrHashMismatch
				}
			} else {
				out = append(out, e)
				seen[e.ID] = e
			}
		}
		return nil
	}()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	return domain.OrderHistoryEvents(out)
}

func (s *FileStore) ListHistoryEvents(ctx context.Context, repoID string) (out []domain.HistoryEvent, err error) {
	err = s.withRefMutationLock(ctx, func() error {
		var e error
		out, e = s.listHistoryEventsAndPositions(repoID)
		return e
	})
	return
}
