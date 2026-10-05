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

func (s *FileStore) remoteObservationPath(repo, remote string) (string, error) {
	return s.scopedRemoteObservationPath(repo, remote, "")
}

func (s *FileStore) scopedRemoteObservationPath(repo, remote, branch string) (string, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return "", err
	}
	if remote == "" {
		return "", domain.ErrInvalidRef
	}
	identity := repo + "\x00" + remote
	if branch != "" {
		if err := domain.ValidateBranchName(branch); err != nil {
			return "", err
		}
		identity += "\x00branch\x00" + branch
	}
	key := domain.HashContent([]byte(identity))
	return filepath.Join(s.storeDir(), "remote-observations", hexOf(key)+".json"), nil
}

func validateRemoteObservation(value outbound.RemoteObservation) error {
	if value.Version != 1 || domain.ValidateContentHash(domain.ContentHash(value.RepoID)) != nil || value.Remote == "" {
		return domain.ErrHashMismatch
	}
	if value.Branch != "" && domain.ValidateBranchName(value.Branch) != nil {
		return domain.ErrInvalidRef
	}
	seen := map[domain.ContentHash]bool{}
	for _, snap := range value.Snapshots {
		if snap.RepoID != value.RepoID || domain.ValidateContentHash(snap.ID) != nil || snap.ID != snap.DocHash || seen[snap.ID] {
			return domain.ErrHashMismatch
		}
		seen[snap.ID] = true
	}
	for _, ref := range value.Refs {
		if ref.RepoID != value.RepoID {
			return domain.ErrHashMismatch
		}
		if err := domain.ValidateRef(ref); err != nil {
			return err
		}
	}
	for _, event := range value.History {
		if event.RepoID != value.RepoID {
			return domain.ErrHashMismatch
		}
		if err := domain.ValidateHistoryEvent(event); err != nil {
			return err
		}
	}
	return nil
}

func observationRevision(value outbound.RemoteObservation) (domain.ContentHash, error) {
	value.Revision = ""
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return domain.HashContent(raw), nil
}

func (s *FileStore) ReadRemoteObservation(ctx context.Context, repo, remote string) (outbound.RemoteObservation, error) {
	return s.readRemoteObservation(ctx, repo, remote, "")
}

func (s *FileStore) ReadScopedRemoteObservation(ctx context.Context, repo, remote, branch string) (outbound.RemoteObservation, error) {
	if domain.ValidateBranchName(branch) != nil {
		return outbound.RemoteObservation{}, domain.ErrInvalidRef
	}
	return s.readRemoteObservation(ctx, repo, remote, branch)
}

func (s *FileStore) readRemoteObservation(ctx context.Context, repo, remote, branch string) (outbound.RemoteObservation, error) {
	if err := ctx.Err(); err != nil {
		return outbound.RemoteObservation{}, err
	}
	path, err := s.scopedRemoteObservationPath(repo, remote, branch)
	if err != nil {
		return outbound.RemoteObservation{}, err
	}
	raw, err := readCxtFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return outbound.RemoteObservation{Version: 1, RepoID: repo, Remote: remote, Branch: branch}, nil
	}
	if err != nil {
		return outbound.RemoteObservation{}, err
	}
	var value outbound.RemoteObservation
	if json.Unmarshal(raw, &value) != nil || value.RepoID != repo || value.Remote != remote || value.Branch != branch {
		return value, domain.ErrHashMismatch
	}
	if err := validateRemoteObservation(value); err != nil {
		return value, err
	}
	hash, err := observationRevision(value)
	if err != nil {
		return value, err
	}
	if value.Revision != hash {
		return value, domain.ErrHashMismatch
	}
	return value, nil
}

func (s *FileStore) CompareAndSwapRemoteObservation(ctx context.Context, expected domain.ContentHash, value outbound.RemoteObservation) error {
	if err := validateRemoteObservation(value); err != nil {
		return err
	}
	path, err := s.scopedRemoteObservationPath(value.RepoID, value.Remote, value.Branch)
	if err != nil {
		return err
	}
	return s.withMutationLock(ctx, "remote-observations", filepath.Base(path), func() error {
		current, err := s.readRemoteObservation(ctx, value.RepoID, value.Remote, value.Branch)
		if err != nil {
			return err
		}
		if current.Revision != expected {
			return domain.ErrSyncConflict
		}
		value.Revision, err = observationRevision(value)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return writeAtomic(path, raw)
	})
}

var _ outbound.RemoteObservationStore = (*FileStore)(nil)

var _ outbound.ScopedRemoteObservationStore = (*FileStore)(nil)
