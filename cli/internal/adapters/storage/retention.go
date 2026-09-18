package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func (s *FileStore) WithObjectsRetained(ctx context.Context, fn func() error) error {
	_, err := s.withOSLock(ctx, "object-retention", "repo", syscall.LOCK_SH, true, fn)
	return err
}

func (s *FileStore) TryCollectObjects(ctx context.Context, fn func() error) (bool, error) {
	return s.withOSLock(ctx, "object-retention", "repo", syscall.LOCK_EX, false, fn)
}

func (s *FileStore) captureCollectionPath(job outbound.CaptureCollection) (string, error) {
	if err := validateHashes(domain.ContentHash(job.RepoID), job.Previous, job.Replacement); err != nil {
		return "", err
	}
	if job.Previous == job.Replacement {
		return "", domain.ErrInvalidCIR
	}
	raw, _ := json.Marshal(job)
	return filepath.Join(s.storeDir(), "capture-collection", hexOf(domain.HashContent(raw))+".json"), nil
}

func (s *FileStore) QueueCaptureCollection(_ context.Context, job outbound.CaptureCollection) error {
	path, err := s.captureCollectionPath(job)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return writeAtomic(path, raw)
}

func (s *FileStore) CaptureCollections(ctx context.Context, repo string, limit int) ([]outbound.CaptureCollection, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return nil, err
	}
	entries, err := readCxtDir(filepath.Join(s.storeDir(), "capture-collection"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []outbound.CaptureCollection
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(s.storeDir(), "capture-collection", entry.Name())
		raw, err := readCxtFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var job outbound.CaptureCollection
		if json.Unmarshal(raw, &job) != nil {
			return nil, domain.ErrHashMismatch
		}
		expected, err := s.captureCollectionPath(job)
		if err != nil || expected != path {
			return nil, domain.ErrHashMismatch
		}
		if job.RepoID == repo {
			out = append(out, job)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *FileStore) CompleteCaptureCollection(_ context.Context, job outbound.CaptureCollection) error {
	path, err := s.captureCollectionPath(job)
	if err != nil {
		return err
	}
	return removeCxtFile(path)
}
