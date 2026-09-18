package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"os"
	"path/filepath"
	"sort"
	"time"
)

var _ outbound.GitChangeStore = (*FSStore)(nil)

func (s *FSStore) gitChangePath(repo domain.ContentHash, id string) string {
	return filepath.Join(s.dataDir, "git-changes", opaqueName(string(repo)+":"+id)+".json")
}
func (s *FSStore) writeGitChange(j domain.GitChangeJob) error {
	if err := j.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return writeAtomic(s.gitChangePath(j.RepoID, j.ID), b)
}
func (s *FSStore) GetGitChange(ctx context.Context, repo domain.ContentHash, id string) (domain.GitChangeJob, error) {
	var j domain.GitChangeJob
	err := readJSON(s.gitChangePath(repo, id), &j)
	if err == nil {
		err = j.Validate()
		if j.RepoID != repo || j.ID != id {
			err = domain.ErrIntegrity
		}
	}
	return j, err
}
func (s *FSStore) EnqueueGitChange(ctx context.Context, j domain.GitChangeJob) (domain.GitChangeJob, error) {
	if err := j.Validate(); err != nil {
		return j, err
	}
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	old, err := s.GetGitChange(ctx, j.RepoID, j.ID)
	if err == nil {
		if old.Request != j.Request || old.GitOrigin != j.GitOrigin {
			return old, domain.ErrConflict
		}
		return old, nil
	}
	if !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, os.ErrNotExist) {
		return j, err
	}
	return j, s.writeGitChange(j)
}
func (s *FSStore) gitChangesRaw() ([]domain.GitChangeJob, error) {
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "git-changes"))
	if errors.Is(err, os.ErrNotExist) {
		return []domain.GitChangeJob{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []domain.GitChangeJob{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var j domain.GitChangeJob
		if err := readJSON(filepath.Join(s.dataDir, "git-changes", e.Name()), &j); err != nil {
			return nil, err
		}
		if err := j.Validate(); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}
func (s *FSStore) ListGitChanges(ctx context.Context, repo domain.ContentHash, after string, limit int) ([]domain.GitChangeSummary, error) {
	if limit < 1 || limit > 101 {
		return nil, domain.ErrValidation
	}
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	jobs, err := s.gitChangesRaw()
	if err != nil {
		return nil, err
	}
	out := []domain.GitChangeSummary{}
	for _, j := range jobs {
		if j.RepoID == repo && j.ID > after {
			out = append(out, j.Summary())
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out[:min(limit, len(out))], nil
}
func (s *FSStore) ClaimGitChange(ctx context.Context, repo domain.ContentHash, id string, now time.Time, lease time.Duration) (domain.GitChangeJob, error) {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	jobs, err := s.gitChangesRaw()
	if err != nil {
		return domain.GitChangeJob{}, err
	}
	sort.Slice(jobs, func(i, k int) bool {
		if !jobs[i].NextAttempt.Equal(jobs[k].NextAttempt) {
			return jobs[i].NextAttempt.Before(jobs[k].NextAttempt)
		}
		return jobs[i].ID < jobs[k].ID
	})
	for _, j := range jobs {
		if (repo != "" && j.RepoID != repo) || (id != "" && j.ID != id) {
			continue
		}
		next, err := j.Claim(now, lease)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return j, err
		}
		return next, s.writeGitChange(next)
	}
	return domain.GitChangeJob{}, domain.ErrNotFound
}
func (s *FSStore) FinishGitChange(ctx context.Context, j domain.GitChangeJob) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	old, err := s.GetGitChange(ctx, j.RepoID, j.ID)
	if err != nil {
		return err
	}
	if err = old.AcceptFinish(j); err != nil {
		return err
	}
	return s.writeGitChange(j)
}
func (s *FSStore) RetryGitChange(ctx context.Context, repo domain.ContentHash, id string, now time.Time) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	j, err := s.GetGitChange(ctx, repo, id)
	if err != nil {
		return err
	}
	j, err = j.Retry(now)
	if err != nil {
		return err
	}
	return s.writeGitChange(j)
}
