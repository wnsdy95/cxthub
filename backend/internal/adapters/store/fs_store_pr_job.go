package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// FS jobs use the same process-wide root lock as other development store jobs.
// Production workers use PostgreSQL row locks, never this single-host adapter.
func (s *FSStore) prJobPath(repo domain.ContentHash, id string) string {
	return filepath.Join(s.dataDir, "pr-jobs", opaqueName(string(repo)+":"+id)+".json")
}
func (s *FSStore) writePRJob(j domain.PRPromotionJob) error {
	if err := j.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return writeAtomic(s.prJobPath(j.RepoID, j.ID), b)
}
func (s *FSStore) EnqueuePRJob(ctx context.Context, j domain.PRPromotionJob) (domain.PRPromotionJob, error) {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	var old domain.PRPromotionJob
	err := readJSON(s.prJobPath(j.RepoID, j.ID), &old)
	if err == nil {
		if old.PR != j.PR || old.GitOrigin != j.GitOrigin {
			return old, domain.ErrConflict
		}
		return old, nil
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, domain.ErrNotFound) {
		return j, err
	}
	return j, s.writePRJob(j)
}
func (s *FSStore) listPRJobsRaw() (jobs []domain.PRPromotionJob, err error) {
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "pr-jobs"))
	if errors.Is(err, os.ErrNotExist) {
		return []domain.PRPromotionJob{}, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var j domain.PRPromotionJob
		if err := readJSON(filepath.Join(s.dataDir, "pr-jobs", e.Name()), &j); err != nil {
			return nil, err
		}
		if err := j.Validate(); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(i, k int) bool {
		if jobs[i].CreatedAt.Equal(jobs[k].CreatedAt) {
			return jobs[i].ID < jobs[k].ID
		}
		return jobs[i].CreatedAt.Before(jobs[k].CreatedAt)
	})
	return jobs, nil
}
func (s *FSStore) ListPRJobs(ctx context.Context, repo domain.ContentHash) ([]domain.PRPromotionJob, error) {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	jobs, err := s.listPRJobsRaw()
	if err != nil {
		return nil, err
	}
	out := []domain.PRPromotionJob{}
	for i := len(jobs) - 1; i >= 0; i-- {
		if jobs[i].RepoID == repo {
			out = append(out, jobs[i])
			if len(out) == 100 {
				break
			}
		}
	}
	return out, nil
}
func (s *FSStore) ClaimPRJob(ctx context.Context, repo domain.ContentHash, id string, now time.Time, lease time.Duration) (domain.PRPromotionJob, error) {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	jobs, err := s.listPRJobsRaw()
	if err != nil {
		return domain.PRPromotionJob{}, err
	}
	running := map[domain.ContentHash]string{}
	for _, j := range jobs {
		if j.State == "running" {
			running[j.RepoID] = j.ID
		}
	}
	blocked := map[domain.ContentHash]bool{}
	for _, j := range jobs {
		if j.State == "completed" || j.State == "attention" {
			continue
		}
		// A woken older job cannot overlap a newer running job. Recover an
		// expired running lease before resuming FIFO among waiting jobs.
		if active := running[j.RepoID]; active != "" && active != j.ID {
			continue
		}
		// Backoff delays this job, not unrelated ready work in its repository.
		// Running leases still reserve the repository via the check above.
		if j.NextAttempt.After(now) {
			continue
		}
		if blocked[j.RepoID] {
			continue
		}
		blocked[j.RepoID] = true
		if (repo != "" && j.RepoID != repo) || (id != "" && j.ID != id) || (j.State == "running" && j.LeaseUntil.After(now)) {
			continue
		}
		j.State = "running"
		j.Attempts++
		j.Version++
		j.UpdatedAt = now
		j.LeaseUntil = now.Add(lease)
		return j, s.writePRJob(j)
	}
	return domain.PRPromotionJob{}, domain.ErrNotFound
}
func (s *FSStore) FinishPRJob(ctx context.Context, j domain.PRPromotionJob) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	var old domain.PRPromotionJob
	if err := readJSON(s.prJobPath(j.RepoID, j.ID), &old); err != nil {
		return err
	}
	if old.Version != j.Version || old.State != "running" || old.PR != j.PR || old.GitOrigin != j.GitOrigin {
		return domain.ErrConflict
	}
	return s.writePRJob(j)
}
func (s *FSStore) RetryPRJob(ctx context.Context, repo domain.ContentHash, id string, now time.Time) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	var j domain.PRPromotionJob
	if err := readJSON(s.prJobPath(repo, id), &j); err != nil {
		return err
	}
	if j.State == "completed" {
		return nil
	}
	if j.State == "running" && j.LeaseUntil.After(now) {
		return domain.ErrConflict
	}
	j.State = "waiting"
	j.NextAttempt = now
	j.UpdatedAt = now
	j.Version++
	j.Attempts = 0
	j.Reason = ""
	return s.writePRJob(j)
}

func (s *FSStore) GetPRJob(ctx context.Context, repo domain.ContentHash, id string) (domain.PRPromotionJob, error) {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	var j domain.PRPromotionJob
	err := readJSON(s.prJobPath(repo, id), &j)
	if errors.Is(err, os.ErrNotExist) {
		err = domain.ErrNotFound
	}
	return j, err
}

func (s *FSStore) WakePRSourceJobs(ctx context.Context, repo domain.ContentHash, now time.Time) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	jobs, err := s.listPRJobsRaw()
	if err != nil {
		return err
	}
	history := map[domain.ContentHash][]domain.HistoryEvent{}
	for _, j := range jobs {
		if j.State != "attention" || j.Reason != "source_finalization_required" || (repo != "" && j.RepoID != repo) {
			continue
		}
		events, ok := history[j.RepoID]
		if !ok {
			events, err = s.ListHistoryEvents(ctx, j.RepoID)
			if err != nil {
				return err
			}
			history[j.RepoID] = events
		}
		if !hasPRSourcePublication(j, events) {
			continue
		}
		j.State, j.Reason, j.Attempts = "waiting", "", 0
		j.NextAttempt, j.UpdatedAt, j.LeaseUntil = now, now, time.Time{}
		j.Version++
		if err := s.writePRJob(j); err != nil {
			return err
		}
	}
	return nil
}
