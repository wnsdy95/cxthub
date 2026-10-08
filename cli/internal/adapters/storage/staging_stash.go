package storage

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ outbound.StagingStashStore = (*FileStore)(nil)

func (s *FileStore) stagingStashPath(id string) (string, error) {
	if len(id) != 32 || id != strings.ToLower(id) {
		return "", domain.ErrHashMismatch
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", domain.ErrHashMismatch
	}
	return filepath.Join(s.storeDir(), "worktrees", s.worktreeID, "index-stashes", id+".json"), nil
}
func (s *FileStore) validateStagingStash(stash domain.StagingStash) error {
	if stash.Version != stash.Index.RecordVersion() {
		return domain.ErrStagingVersion
	}
	if _, err := s.stagingStashPath(stash.ID); err != nil {
		return err
	}
	if err := domain.ValidateStagingIndex(stash.Index); err != nil {
		return err
	}
	if stash.Index.WorktreeID != s.worktreeID || stash.Position.WorktreeID != s.worktreeID || stash.Position.RepoID != stash.Index.RepoID || stash.CreatedAt.IsZero() {
		return domain.ErrHashMismatch
	}
	return nil
}
func (s *FileStore) writeStagingStash(stash domain.StagingStash) error {
	if err := s.validateStagingStash(stash); err != nil {
		return err
	}
	path, _ := s.stagingStashPath(stash.ID)
	raw, err := json.Marshal(stash)
	if err != nil {
		return err
	}
	return writeAtomic(path, raw)
}
func (s *FileStore) readStagingStash(repo, id string) (domain.StagingStash, error) {
	path, err := s.stagingStashPath(id)
	if err != nil {
		return domain.StagingStash{}, err
	}
	raw, err := readCxtFile(path)
	if os.IsNotExist(err) {
		return domain.StagingStash{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.StagingStash{}, err
	}
	var stash domain.StagingStash
	if json.Unmarshal(raw, &stash) != nil {
		return stash, domain.ErrHashMismatch
	}
	if err = s.validateStagingStash(stash); err != nil {
		return stash, err
	}
	if stash.ID != id || stash.Index.RepoID != repo {
		return stash, domain.ErrHashMismatch
	}
	return stash, nil
}
func (s *FileStore) StashIndex(ctx context.Context, stash domain.StagingStash) (domain.StagingStash, error) {
	if err := s.validateStagingStash(stash); err != nil {
		return stash, err
	}
	if len(stash.Index.Entries) == 0 {
		return stash, domain.ErrEmptyIndex
	}
	if stash.Applied {
		return stash, domain.ErrHashMismatch
	}
	err := s.WithObjectsRetained(ctx, func() error {
		return s.withRefMutationLock(ctx, func() error {
			if err := s.reconcileAppliedStaging(ctx, stash.Index.RepoID); err != nil {
				return err
			}
			current, err := s.readStagingIndex(stash.Index.RepoID)
			if err != nil {
				return err
			}
			if current.Revision != stash.Index.Revision {
				return domain.ErrSyncConflict
			}
			p, err := s.stagingPosition(ctx, stash.Index.RepoID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(p, stash.Position) {
				return domain.ErrSelectionChanged
			}
			if err := s.verifyStagedDocs(ctx, stash.Index); err != nil {
				return err
			}
			prior, err := s.readStagingStash(stash.Index.RepoID, stash.ID)
			if err == nil && !reflect.DeepEqual(prior, stash) {
				return domain.ErrSyncConflict
			}
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return err
			}
			// Persist recovery ownership before clearing the index. A crash can leave a
			// duplicate reference, never an index whose only source was lost.
			if err := s.writeStagingStash(stash); err != nil {
				return err
			}
			return s.writeStaging(domain.ConsumeStagedEntries(current, stash.Index.Entries))
		})
	})
	return stash, err
}
func (s *FileStore) PopIndex(ctx context.Context, repo, id string, expected domain.ContentHash, position domain.WorkingPosition) (result domain.StagingIndex, err error) {
	err = s.WithObjectsRetained(ctx, func() error {
		return s.withRefMutationLock(ctx, func() error {
			stash, err := s.readStagingStash(repo, id)
			if err != nil {
				return err
			}
			current, err := s.readStagingIndex(repo)
			if err != nil {
				return err
			}
			if current.Revision != expected {
				return domain.ErrSyncConflict
			}
			p, err := s.stagingPosition(ctx, repo)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(p, position) {
				return domain.ErrSelectionChanged
			}
			if stash.Applied {
				result = current
				return nil
			}
			if p.GitCommit != stash.Position.GitCommit || p.BranchID != stash.Position.BranchID || p.Branch != stash.Position.Branch {
				return domain.ErrSelectionChanged
			}
			if err := s.verifyStagedDocs(ctx, stash.Index); err != nil {
				return err
			}
			next := current
			next.Entries = append([]domain.StagedSession{}, current.Entries...)
			byKey := map[domain.ContentHash]domain.StagedSession{}
			for _, entry := range current.Entries {
				byKey[entry.Key] = entry
			}
			for _, entry := range stash.Index.Entries {
				if prior, ok := byKey[entry.Key]; ok {
					if prior != entry {
						return domain.ErrSyncConflict
					}
					continue
				}
				next.Entries = append(next.Entries, entry)
			}
			if len(next.Entries) != len(current.Entries) {
				next.Sequence++
				next = next.WithRevision()
				if err := s.writeStaging(next); err != nil {
					return err
				}
			}
			// If this acknowledgement fails, repeating pop sees identical entries and
			// acknowledges them. A conflicting re-add always preserves the saved stash.
			stash.Applied = true
			if err := s.writeStagingStash(stash); err != nil {
				return err
			}
			result = next
			return nil
		})
	})
	return
}
func (s *FileStore) ListIndexStashes(ctx context.Context, repo string) ([]domain.StagingStash, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.storeDir(), "worktrees", s.worktreeID, "index-stashes")
	entries, err := readCxtDir(dir)
	if os.IsNotExist(err) {
		return []domain.StagingStash{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := []domain.StagingStash{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, domain.ErrHashMismatch
		}
		stash, err := s.readStagingStash(repo, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if !stash.Applied {
			result = append(result, stash)
		}
	}
	return result, nil
}
