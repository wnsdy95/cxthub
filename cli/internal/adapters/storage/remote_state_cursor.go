package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

const remoteSnapshotStateCursorVersion = 1

type remoteSnapshotStateCursorFile struct {
	Version int                                                          `json:"version"`
	RepoID  string                                                       `json:"repo_id"`
	Entries map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry `json:"entries"`
}

func (s *FileStore) remoteSnapshotStateCursorPath(repoID string) (string, error) {
	id := domain.ContentHash(repoID)
	if err := domain.ValidateContentHash(id); err != nil {
		return "", err
	}
	return filepath.Join(s.storeDir(), "remote-state-cursors", hexOf(id)+".json"), nil
}

// LoadRemoteSnapshotStateCursor reads a validated, repo-bound pull hint. A
// missing file is an empty cursor; malformed content is never trusted.
func (s *FileStore) LoadRemoteSnapshotStateCursor(_ context.Context, repoID string) (map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry, error) {
	path, err := s.remoteSnapshotStateCursorPath(repoID)
	if err != nil {
		return nil, err
	}
	raw, err := readCxtFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	var file remoteSnapshotStateCursorFile
	if json.Unmarshal(raw, &file) != nil || file.Version != remoteSnapshotStateCursorVersion || file.RepoID != repoID {
		return nil, domain.ErrHashMismatch
	}
	out := make(map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry, len(file.Entries))
	for id, entry := range file.Entries {
		if domain.ValidateContentHash(id) != nil || domain.ValidateContentHash(entry.LocalState) != nil || domain.ValidateContentHash(entry.RemoteState) != nil || entry.LocalState == entry.RemoteState {
			return nil, domain.ErrHashMismatch
		}
		out[id] = entry
	}
	return out, nil
}

// SaveRemoteSnapshotStateCursor atomically replaces the cursor. Equal local
// and remote states need no hint and are omitted. Empty cursors remove the file.
func (s *FileStore) SaveRemoteSnapshotStateCursor(_ context.Context, repoID string, entries map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry) error {
	path, err := s.remoteSnapshotStateCursorPath(repoID)
	if err != nil {
		return err
	}
	clean := make(map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry, len(entries))
	for id, entry := range entries {
		if domain.ValidateContentHash(id) != nil || domain.ValidateContentHash(entry.LocalState) != nil || domain.ValidateContentHash(entry.RemoteState) != nil {
			return domain.ErrHashMismatch
		}
		if entry.LocalState != entry.RemoteState {
			clean[id] = entry
		}
	}
	if len(clean) == 0 {
		return removeCxtFile(path)
	}
	raw, err := json.Marshal(remoteSnapshotStateCursorFile{
		Version: remoteSnapshotStateCursorVersion,
		RepoID:  repoID,
		Entries: clean,
	})
	if err != nil {
		return err
	}
	return writeAtomic(path, raw)
}

var _ outbound.RemoteSnapshotStateCursorStore = (*FileStore)(nil)
