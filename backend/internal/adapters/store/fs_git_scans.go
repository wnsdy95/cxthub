package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"
)

var _ outbound.GitScanStore = (*FSStore)(nil)

func (s *FSStore) gitEvidencePath(kind string, repo domain.ContentHash, id string) string {
	return filepath.Join(s.dataDir, kind, opaqueName(string(repo)+":"+id)+".json")
}
func (s *FSStore) writeGitScan(j domain.GitScanJob) error {
	if err := j.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return writeAtomic(s.gitEvidencePath("git-scans", j.RepoID, j.ID), b)
}
func (s *FSStore) GetGitScan(ctx context.Context, repo domain.ContentHash, id string) (domain.GitScanJob, error) {
	var j domain.GitScanJob
	err := readJSON(s.gitEvidencePath("git-scans", repo, id), &j)
	if err == nil {
		err = j.Validate()
		if j.RepoID != repo || j.ID != id {
			err = domain.ErrIntegrity
		}
	}
	return j, err
}
func (s *FSStore) enqueueGitScan(ctx context.Context, j domain.GitScanJob) error {
	if err := j.Validate(); err != nil {
		return err
	}
	_, err := s.GetGitScan(ctx, j.RepoID, j.ID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.writeGitScan(j)
}
func (s *FSStore) EnqueueGitScan(ctx context.Context, j domain.GitScanJob) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	return s.enqueueGitScan(ctx, j)
}
func readGitEvidenceFS[T any](dir string) ([]T, error) {
	entries, err := os.ReadDir(dir)
	out := []T{}
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var v T
		if err = readJSON(filepath.Join(dir, e.Name()), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *FSStore) ListGitScans(ctx context.Context, repo domain.ContentHash, after string, limit int) ([]domain.GitScanJob, error) {
	if limit < 1 || limit > 101 {
		return nil, domain.ErrValidation
	}
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	all, err := readGitEvidenceFS[domain.GitScanJob](filepath.Join(s.dataDir, "git-scans"))
	if err != nil {
		return nil, err
	}
	out := []domain.GitScanJob{}
	for _, j := range all {
		if err = j.Validate(); err != nil {
			return nil, err
		}
		if j.RepoID == repo && j.ID > after {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out[:min(limit, len(out))], nil
}
func (s *FSStore) ClaimGitScan(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.GitScanJob, error) {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	all, err := readGitEvidenceFS[domain.GitScanJob](filepath.Join(s.dataDir, "git-scans"))
	if err != nil {
		return domain.GitScanJob{}, err
	}
	sort.Slice(all, func(i, k int) bool {
		if all[i].NextAttempt.Equal(all[k].NextAttempt) {
			return all[i].ID < all[k].ID
		}
		return all[i].NextAttempt.Before(all[k].NextAttempt)
	})
	for _, j := range all {
		if repo != "" && j.RepoID != repo {
			continue
		}
		n, err := j.Claim(now, lease)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return j, err
		}
		return n, s.writeGitScan(n)
	}
	return domain.GitScanJob{}, domain.ErrNotFound
}
func (s *FSStore) FinishGitScan(ctx context.Context, p domain.GitScanFinish) error {
	if err := p.Validate(); err != nil {
		return err
	}
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	old, err := s.GetGitScan(ctx, p.Job.RepoID, p.Job.ID)
	if err != nil {
		return err
	}
	if err = p.ValidateFor(old); err != nil {
		return err
	}
	if p.Tree != nil {
		if err = s.writeGitTreeFS(p.Job.RepoID, p.Job.GitOrigin, *p.Tree); err != nil {
			return err
		}
	}
	// Development FS is one-process only. Publish idempotent dependencies first,
	// cursor last: a crash replays the leased page; it cannot skip unqueued work.
	for _, d := range p.Deltas {
		path := s.gitEvidencePath("git-deltas", d.RepoID, d.ID)
		var existing domain.GitDeltaRecord
		err = readJSON(path, &existing)
		if err == nil {
			if !reflect.DeepEqual(existing, d) {
				return domain.ErrIntegrity
			}
			continue
		}
		if !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		b, err := json.Marshal(d)
		if err != nil {
			return err
		}
		if err = writeAtomic(path, b); err != nil {
			return err
		}
	}
	if len(p.Deltas) > 0 {
		ids := []string{}
		for _, d := range p.Deltas {
			ids = append(ids, d.ID)
		}
		b, e := json.Marshal(ids)
		if e != nil {
			return e
		}
		if e = writeAtomic(s.gitEvidencePath("git-comparisons", p.Job.RepoID, p.Job.GitOrigin+":"+p.Job.Commit), b); e != nil {
			return e
		}
	}
	for _, j := range p.Parents {
		if err = s.enqueueGitScan(ctx, j); err != nil {
			return err
		}
	}
	for _, j := range p.Changes {
		_, err = s.GetGitChange(ctx, j.RepoID, j.ID)
		if err == nil {
			continue
		}
		if !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err = s.writeGitChange(j); err != nil {
			return err
		}
	}
	return s.writeGitScan(p.Job)
}
func (s *FSStore) RetryGitScan(ctx context.Context, repo domain.ContentHash, id string, now time.Time) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	j, err := s.GetGitScan(ctx, repo, id)
	if err != nil {
		return err
	}
	j, err = j.Retry(now)
	if err != nil {
		return err
	}
	return s.writeGitScan(j)
}
func (s *FSStore) FindGitInverses(ctx context.Context, repo domain.ContentHash, origin, commit, after string, limit int) ([]domain.GitInverseCandidate, error) {
	if limit < 1 || limit > 51 {
		return nil, domain.ErrValidation
	}
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	all, err := readGitEvidenceFS[domain.GitDeltaRecord](filepath.Join(s.dataDir, "git-deltas"))
	if err != nil {
		return nil, err
	}
	own := []domain.GitDeltaRecord{}
	others := []domain.GitDeltaRecord{}
	for _, r := range all {
		if err = r.Validate(); err != nil {
			return nil, err
		}
		if r.RepoID != repo || r.GitOrigin != origin {
			continue
		}
		if r.Delta.Commit == commit {
			own = append(own, r)
		} else {
			others = append(others, r)
		}
	}
	out := []domain.GitInverseCandidate{}
	for _, a := range own {
		keys := map[string]bool{}
		for _, k := range a.InverseKeys {
			keys[k] = true
		}
		for _, b := range others {
			cursor := a.ID + b.ID
			if cursor <= after {
				continue
			}
			match := false
			for _, k := range b.Keys {
				if keys[k] {
					match = true
					break
				}
			}
			if match {
				out = append(out, domain.GitInverseCandidate{Cursor: cursor, Request: domain.GitChangeRequest{Target: b.Delta.Commit, TargetParent: b.Delta.Parent, Commit: a.Delta.Commit, Parent: a.Delta.Parent}})
			}
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Cursor < out[k].Cursor })
	return out[:min(limit, len(out))], nil
}
func (s *FSStore) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, commit, parent string) (domain.GitCommitDelta, error) {
	if parent != "" {
		var r domain.GitDeltaRecord
		id := domain.NewGitDelta(repo, origin, domain.GitCommitDelta{Commit: commit, Parent: parent}).ID
		err := readJSON(s.gitEvidencePath("git-deltas", repo, id), &r)
		if err != nil {
			return r.Delta, err
		}
		return r.Delta, r.Validate()
	}
	var ids []string
	if err := readJSON(s.gitEvidencePath("git-comparisons", repo, origin+":"+commit), &ids); err != nil {
		return domain.GitCommitDelta{}, err
	}
	if len(ids) != 1 {
		return domain.GitCommitDelta{}, domain.ErrNotFound
	}
	var r domain.GitDeltaRecord
	if err := readJSON(s.gitEvidencePath("git-deltas", repo, ids[0]), &r); err != nil {
		return r.Delta, err
	}
	if r.RepoID != repo || r.GitOrigin != origin || r.Delta.Commit != commit {
		return r.Delta, domain.ErrIntegrity
	}
	return r.Delta, r.Validate()
}
func (s *FSStore) RecordGitRefObservation(ctx context.Context, o domain.GitRefObservation, now time.Time) error {
	if err := o.Validate(); err != nil {
		return err
	}
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	return s.recordGitObservation(ctx, o, now)
}
func (s *FSStore) recordGitObservation(ctx context.Context, o domain.GitRefObservation, now time.Time) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	// A delivery is acknowledged only after all jobs exist. Retrying the same
	// immutable observation repairs a crash between these atomic file writes.
	var old domain.GitRefObservation
	existing := readJSON(s.gitEvidencePath("git-observations", o.RepoID, o.ID), &old)
	if existing == nil && !reflect.DeepEqual(old, o) {
		return domain.ErrConflict
	}
	if existing != nil && !errors.Is(existing, domain.ErrNotFound) && !errors.Is(existing, os.ErrNotExist) {
		return existing
	}
	if err = writeAtomic(s.gitEvidencePath("git-observations", o.RepoID, o.ID), b); err != nil {
		return err
	}
	for _, sha := range []string{o.Before, o.After} {
		if sha != "" {
			if err = s.enqueueGitScan(ctx, domain.NewGitScan(o.RepoID, o.GitOrigin, sha, now)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *FSStore) ClaimGitHeadScan(ctx context.Context, repo domain.ContentHash, origin string, now time.Time, lease time.Duration) (domain.GitHeadScan, error) {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	j := domain.GitHeadScan{RepoID: repo, GitOrigin: origin, Page: 1, State: "waiting"}
	if err := j.Validate(); err != nil {
		return j, err
	}
	path := s.gitEvidencePath("git-head-scans", repo, origin)
	err := readJSON(path, &j)
	if err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, os.ErrNotExist) {
		return j, err
	}
	j, err = j.Claim(now, lease)
	if err != nil {
		return j, err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return j, err
	}
	return j, writeAtomic(path, b)
}
func (s *FSStore) FinishGitHeadScan(ctx context.Context, j domain.GitHeadScan, observations []domain.GitRefObservation, more bool, now time.Time) error {
	if err := j.Validate(); err != nil {
		return err
	}
	if len(observations) > 100 {
		return domain.ErrValidation
	}
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	path := s.gitEvidencePath("git-head-scans", j.RepoID, j.GitOrigin)
	var old domain.GitHeadScan
	if err := readJSON(path, &old); err != nil {
		return err
	}
	if old.Version != j.Version || old.Page != j.Page || old.LeaseUntil.IsZero() {
		return domain.ErrConflict
	}
	for _, o := range observations {
		if o.RepoID != j.RepoID || o.GitOrigin != j.GitOrigin || o.Source != "reconciliation" {
			return domain.ErrIntegrity
		}
		if err := o.Validate(); err != nil {
			return err
		}
		if err := s.recordGitObservation(ctx, o, now); err != nil {
			return err
		}
	}
	b, err := json.Marshal(j.Finish(more, now))
	if err != nil {
		return err
	}
	return writeAtomic(path, b)
}

var _ outbound.GitHeadScanStore = (*FSStore)(nil)

func (s *FSStore) GetGitHeadScan(ctx context.Context, repo domain.ContentHash, origin string) (domain.GitHeadScan, error) {
	var j domain.GitHeadScan
	err := readJSON(s.gitEvidencePath("git-head-scans", repo, origin), &j)
	if err != nil {
		return j, err
	}
	if j.RepoID != repo || j.GitOrigin != origin {
		return j, domain.ErrIntegrity
	}
	return j, j.Validate()
}
func (s *FSStore) FailGitHeadScan(ctx context.Context, j domain.GitHeadScan, now time.Time) error {
	if err := j.Validate(); err != nil {
		return err
	}
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	old, err := s.GetGitHeadScan(ctx, j.RepoID, j.GitOrigin)
	if err != nil {
		return err
	}
	if old.Version != j.Version || old.Page != j.Page || old.State != "running" {
		return domain.ErrConflict
	}
	b, err := json.Marshal(j.Defer(now))
	if err != nil {
		return err
	}
	return writeAtomic(s.gitEvidencePath("git-head-scans", j.RepoID, j.GitOrigin), b)
}
