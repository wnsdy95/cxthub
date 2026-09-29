package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func (s *FileStore) personalWorkPath(repo string, scope domain.PersonalWorkScope) (string, error) {
	if domain.ValidateContentHash(domain.ContentHash(repo)) != nil || !scope.Complete() || scope.WorktreeID != s.worktreeID {
		return "", domain.ErrHashMismatch
	}
	raw, err := json.Marshal(struct {
		Repo  string
		Scope domain.PersonalWorkScope
	}{repo, scope})
	if err != nil {
		return "", err
	}
	return filepath.Join(s.storeDir(), "personal-work", hexOf(domain.HashContent(raw))+".json"), nil
}

func (s *FileStore) ReadPersonalWork(ctx context.Context, repo string, scope domain.PersonalWorkScope) (domain.PersonalWorkState, error) {
	var out domain.PersonalWorkState
	if err := ctx.Err(); err != nil {
		return out, err
	}
	path, err := s.personalWorkPath(repo, scope)
	if err != nil {
		return out, err
	}
	raw, err := readCxtFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, domain.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	var stored struct {
		Version    int
		Repository string
		State      domain.PersonalWorkState
		Hash       domain.ContentHash
	}
	if len(raw) > 1<<20 || json.Unmarshal(raw, &stored) != nil || stored.Version != 1 || stored.Repository != repo || stored.State.Scope != scope {
		return out, domain.ErrHashMismatch
	}
	data, err := json.Marshal(stored.State)
	if err != nil || domain.HashContent(data) != stored.Hash {
		return out, domain.ErrHashMismatch
	}
	return stored.State, nil
}

// PutPersonalWork persists explicitly supplied structured work state. It never
// derives a user's instructions or approvals from shared transcript prose.
func (s *FileStore) PutPersonalWork(ctx context.Context, repo string, state domain.PersonalWorkState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.personalWorkPath(repo, state.Scope)
	if err != nil {
		return err
	}
	if len(state.Sources) == 0 {
		return domain.ErrHashMismatch
	}
	for _, source := range state.Sources {
		snap, err := s.GetSnapshot(ctx, source.SnapshotID)
		if err != nil {
			return err
		}
		if snap.RepoID != repo {
			return domain.ErrHashMismatch
		}
	}
	for _, constraint := range state.Constraints {
		if constraint.Text == "" || domain.ValidateContentHash(constraint.Source.SnapshotID) != nil {
			return domain.ErrHashMismatch
		}
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return domain.ErrHashMismatch
	}
	stored, err := json.Marshal(struct {
		Version    int
		Repository string
		State      domain.PersonalWorkState
		Hash       domain.ContentHash
	}{1, repo, state, domain.HashContent(raw)})
	if err != nil {
		return err
	}
	return writeAtomic(path, stored)
}
