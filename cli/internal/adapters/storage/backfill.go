package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ outbound.HistoricalBackfillStore = (*FileStore)(nil)

func (s *FileStore) backfillPath(repo string, id domain.ContentHash) (string, error) {
	if err := validateHashes(domain.ContentHash(repo), id); err != nil {
		return "", err
	}
	return filepath.Join(s.storeDir(), "historical-backfill", hexOf(domain.ContentHash(repo)), hexOf(id)+".json"), nil
}

func readBackfill(path string) (domain.SnapshotBackfill, error) {
	var job domain.SnapshotBackfill
	raw, err := readCxtFile(path)
	if err != nil {
		return job, err
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return job, domain.ErrHashMismatch
	}
	if err := job.Validate(); err != nil {
		return job, err
	}
	if filepath.Base(path) != hexOf(job.Snapshot)+".json" || filepath.Base(filepath.Dir(path)) != hexOf(domain.ContentHash(job.RepoID)) {
		return job, domain.ErrHashMismatch
	}
	return job, nil
}

func (s *FileStore) StageBackfills(ctx context.Context, repo string, snapshots []domain.Snapshot) error {
	_, err := s.withOSLock(ctx, "historical-backfill", "queue", syscall.LOCK_EX, true, func() error {
		for _, snapshot := range snapshots {
			if err := ctx.Err(); err != nil {
				return err
			}
			if snapshot.RepoID != repo || snapshot.ID != snapshot.DocHash {
				return domain.ErrHashMismatch
			}
			path, err := s.backfillPath(repo, snapshot.ID)
			if err != nil {
				return err
			}
			state, err := domain.SnapshotStateHash(snapshot)
			if err != nil {
				return err
			}
			now := time.Now().UTC()
			job := domain.SnapshotBackfill{RepoID: repo, Snapshot: snapshot.ID, StateHash: state, Version: 1, CreatedAt: now, NextAttempt: now}
			old, err := readBackfill(path)
			if err == nil {
				if old.StateHash == state {
					continue
				}
				job.CreatedAt, job.Version = old.CreatedAt, old.Version+1
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := job.Validate(); err != nil {
				return err
			}
			raw, err := json.Marshal(job)
			if err != nil {
				return err
			}
			if err := writeAtomic(path, raw); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

func (s *FileStore) ListBackfills(ctx context.Context, repo string) ([]domain.SnapshotBackfill, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.storeDir(), "historical-backfill", hexOf(domain.ContentHash(repo)))
	entries, err := readCxtDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []domain.SnapshotBackfill{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []domain.SnapshotBackfill{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		job, err := readBackfill(filepath.Join(dir, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		} // acknowledged after directory read
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].Snapshot < out[j].Snapshot
	})
	return out, nil
}

func (s *FileStore) HasBackfillPin(ctx context.Context, id domain.ContentHash) (bool, error) {
	if err := domain.ValidateContentHash(id); err != nil {
		return false, err
	}
	dir := filepath.Join(s.storeDir(), "historical-backfill")
	repos, err := readCxtDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if strings.HasPrefix(repo.Name(), ".") {
			continue
		}
		if _, ok := hashFromObjectName(repo.Name()); !ok || !repo.IsDir() {
			return false, domain.ErrHashMismatch
		}
		_, err := readBackfill(filepath.Join(dir, repo.Name(), hexOf(id)+".json"))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

// A changed queue version is newer work, never acknowledged by a delayed
// worker. Removing a job is durable; a crash before directory sync can at worst
// repeat an idempotent upload, never lose an unacknowledged body.
func (s *FileStore) UpdateBackfill(ctx context.Context, expected domain.SnapshotBackfill, next *domain.SnapshotBackfill) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	if next != nil {
		if err := next.Validate(); err != nil {
			return err
		}
		if next.RepoID != expected.RepoID || next.Snapshot != expected.Snapshot || next.StateHash != expected.StateHash || next.Version != expected.Version+1 || !next.CreatedAt.Equal(expected.CreatedAt) {
			return domain.ErrHashMismatch
		}
	}
	_, err := s.withOSLock(ctx, "historical-backfill", "queue", syscall.LOCK_EX, true, func() error {
		path, err := s.backfillPath(expected.RepoID, expected.Snapshot)
		if err != nil {
			return err
		}
		old, err := readBackfill(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(old, expected) {
			return nil
		}
		if next == nil {
			if err := removeCxtFile(path); err != nil {
				return err
			}
			return syncCxtParents(path)
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		return writeAtomic(path, raw)
	})
	return err
}

func (s *FileStore) WithBackfillWorker(ctx context.Context, fn func() error) (bool, error) {
	return s.withOSLock(ctx, "historical-backfill", "worker", syscall.LOCK_EX, false, fn)
}

// A detached launcher keeps this separate lock while sleeping for retries.
// Manual batch workers still share WithBackfillWorker's publication exclusion.
func (s *FileStore) WithBackfillDaemon(ctx context.Context, fn func() error) (bool, error) {
	return s.withOSLock(ctx, "historical-backfill", "daemon", syscall.LOCK_EX, false, fn)
}
