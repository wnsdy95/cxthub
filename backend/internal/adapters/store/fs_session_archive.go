package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.SessionArchiveStore = (*FSStore)(nil)

var fsSessionArchiveLocks sync.Map

func validateSessionArchive(record domain.SessionArchive) error {
	if err := validateHashes(record.RepoID, record.Key, record.SnapshotID); err != nil {
		return err
	}
	if record.Key != domain.SessionArchiveKey(domain.Snapshot{ID: record.SnapshotID, Provider: record.Provider, SessionID: record.SessionID}) {
		return fmt.Errorf("%w: session archive key mismatch", domain.ErrIntegrity)
	}
	if record.ArchivedAt.IsZero() || strings.TrimSpace(record.ArchivedBy) == "" {
		return fmt.Errorf("%w: session archive attribution required", domain.ErrValidation)
	}
	return nil
}

func (s *FSStore) sessionArchiveLock(repo domain.ContentHash) *sync.Mutex {
	lock, _ := fsSessionArchiveLocks.LoadOrStore(s.dataDir+"\x00"+string(repo), &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *FSStore) sessionArchivePath(repo, key domain.ContentHash) string {
	return filepath.Join(s.repoDir(repo), "session-archives", hexOf(key)+".json")
}

func (s *FSStore) readSessionArchive(repo, key domain.ContentHash) (domain.SessionArchive, error) {
	var record domain.SessionArchive
	path := s.sessionArchivePath(repo, key)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return record, domain.ErrNotFound
	}
	if err != nil {
		return record, err
	}
	if !info.Mode().IsRegular() {
		return record, fmt.Errorf("%w: session archive is not a regular file", domain.ErrIntegrity)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return record, err
	}
	if err = json.Unmarshal(raw, &record); err != nil {
		return record, fmt.Errorf("%w: invalid session archive JSON: %v", domain.ErrIntegrity, err)
	}
	if err = validateSessionArchive(record); err != nil {
		return record, fmt.Errorf("%w: invalid stored session archive: %v", domain.ErrIntegrity, err)
	}
	if record.RepoID != repo || record.Key != key {
		return record, fmt.Errorf("%w: session archive path mismatch", domain.ErrIntegrity)
	}
	return record, nil
}

func (s *FSStore) ListSessionArchives(ctx context.Context, repo domain.ContentHash) ([]domain.SessionArchive, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	lock := s.sessionArchiveLock(repo)
	lock.Lock()
	defer lock.Unlock()
	return s.listSessionArchives(ctx, repo)
}

func (s *FSStore) listSessionArchives(ctx context.Context, repo domain.ContentHash) ([]domain.SessionArchive, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.repoDir(repo), "session-archives"))
	records := []domain.SessionArchive{}
	if errors.Is(err, os.ErrNotExist) {
		return records, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") && strings.Contains(entry.Name(), ".json.tmp-") {
			continue
		}
		key := domain.ContentHash("sha256:" + strings.TrimSuffix(entry.Name(), ".json"))
		if !strings.HasSuffix(entry.Name(), ".json") || validateHash(key) != nil {
			return nil, fmt.Errorf("%w: invalid session archive filename", domain.ErrIntegrity)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := s.readSessionArchive(repo, key)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	sort.Slice(records, func(left, right int) bool { return records[left].Key < records[right].Key })
	return records, nil
}

func (s *FSStore) PutSessionArchive(ctx context.Context, record domain.SessionArchive) error {
	if err := validateSessionArchive(record); err != nil {
		return err
	}
	lock := s.sessionArchiveLock(record.RepoID)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.GetSnapshot(ctx, record.RepoID, record.SnapshotID); err != nil {
		return err
	}
	if _, err := s.readSessionArchive(record.RepoID, record.Key); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeAtomic(s.sessionArchivePath(record.RepoID, record.Key), raw)
}

func (s *FSStore) DeleteSessionArchive(ctx context.Context, repo, key domain.ContentHash) error {
	if err := validateHashes(repo, key); err != nil {
		return err
	}
	lock := s.sessionArchiveLock(repo)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.readSessionArchive(repo, key); errors.Is(err, domain.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	return removeFileDurable(s.sessionArchivePath(repo, key))
}
