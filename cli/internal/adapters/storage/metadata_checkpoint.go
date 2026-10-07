package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

const metadataCheckpointNamespace = "metadata-checkpoints"

func (s *FileStore) metadataCheckpointPath(repo, remote string) (string, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return "", err
	}
	if remote == "" {
		return "", domain.ErrInvalidRef
	}
	key := domain.HashContent([]byte(repo + "\x00" + remote))
	return filepath.Join(s.storeDir(), metadataCheckpointNamespace, hexOf(key)+".json"), nil
}

func validateMetadataCheckpoint(ctx context.Context, value outbound.MetadataCheckpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if value.Version != 1 || domain.ValidateContentHash(domain.ContentHash(value.RepoID)) != nil || value.Remote == "" {
		return domain.ErrHashMismatch
	}
	seen := make(map[domain.ContentHash]bool, len(value.Snapshots))
	for _, snap := range value.Snapshots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if snap.RepoID != value.RepoID || seen[snap.ID] {
			return domain.ErrHashMismatch
		}
		// Validate metadata references only; referenced objects may not be fetched.
		if err := validateSnapshotRefs(snap); err != nil {
			return err
		}
		seen[snap.ID] = true
	}
	return ctx.Err()
}

func canonicalMetadataCheckpoint(value outbound.MetadataCheckpoint) (outbound.MetadataCheckpoint, error) {
	value.Snapshots = append([]domain.Snapshot{}, value.Snapshots...)
	sort.Slice(value.Snapshots, func(i, j int) bool {
		return value.Snapshots[i].ID < value.Snapshots[j].ID
	})
	// The revision is a local integrity/CAS token, not proof of remote content.
	// Include every snapshot field but omit the revision field from the payload.
	raw, err := json.Marshal(struct {
		Version   int               `json:"version"`
		RepoID    string            `json:"repo_id"`
		Remote    string            `json:"remote"`
		Snapshots []domain.Snapshot `json:"snapshots"`
	}{value.Version, value.RepoID, value.Remote, value.Snapshots})
	if err != nil {
		return outbound.MetadataCheckpoint{}, err
	}
	value.Revision = domain.HashContent(raw)
	return value, nil
}

func (s *FileStore) ReadMetadataCheckpoint(ctx context.Context, repo, remote string) (outbound.MetadataCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return outbound.MetadataCheckpoint{}, err
	}
	path, err := s.metadataCheckpointPath(repo, remote)
	if err != nil {
		return outbound.MetadataCheckpoint{}, err
	}
	raw, err := readCxtFile(path)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return outbound.MetadataCheckpoint{}, ctxErr
	}
	if errors.Is(err, os.ErrNotExist) {
		return outbound.MetadataCheckpoint{Version: 1, RepoID: repo, Remote: remote}, nil
	}
	if err != nil {
		return outbound.MetadataCheckpoint{}, err
	}
	var value outbound.MetadataCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(json.RawMessage)) != io.EOF || value.RepoID != repo || value.Remote != remote {
		return outbound.MetadataCheckpoint{}, domain.ErrHashMismatch
	}
	if err := validateMetadataCheckpoint(ctx, value); err != nil {
		return outbound.MetadataCheckpoint{}, err
	}
	canonical, err := canonicalMetadataCheckpoint(value)
	if err != nil {
		return outbound.MetadataCheckpoint{}, err
	}
	if err := ctx.Err(); err != nil {
		return outbound.MetadataCheckpoint{}, err
	}
	if value.Revision != canonical.Revision {
		return outbound.MetadataCheckpoint{}, domain.ErrHashMismatch
	}
	return canonical, nil
}

func (s *FileStore) CompareAndSwapMetadataCheckpoint(ctx context.Context, expected domain.ContentHash, next outbound.MetadataCheckpoint) (outbound.MetadataCheckpoint, error) {
	if err := validateMetadataCheckpoint(ctx, next); err != nil {
		return outbound.MetadataCheckpoint{}, err
	}
	if err := domain.ValidateOptionalContentHash(expected); err != nil {
		return outbound.MetadataCheckpoint{}, err
	}
	path, err := s.metadataCheckpointPath(next.RepoID, next.Remote)
	if err != nil {
		return outbound.MetadataCheckpoint{}, err
	}
	var saved outbound.MetadataCheckpoint
	err = s.withMutationLock(ctx, metadataCheckpointNamespace, filepath.Base(path), func() error {
		current, err := s.ReadMetadataCheckpoint(ctx, next.RepoID, next.Remote)
		if err != nil {
			return err
		}
		if current.Revision != expected {
			return domain.ErrSyncConflict
		}
		saved, err = canonicalMetadataCheckpoint(next)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return writeAtomic(path, raw)
	})
	if err != nil {
		return outbound.MetadataCheckpoint{}, err
	}
	return saved, nil
}

var _ outbound.MetadataCheckpointStore = (*FileStore)(nil)
