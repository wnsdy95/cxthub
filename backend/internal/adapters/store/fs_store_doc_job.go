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
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.DocJobStore = (*FSStore)(nil)

func (s *FSStore) docJobPath(repo domain.ContentHash, id string) string {
	return filepath.Join(s.dataDir, "doc-jobs", opaqueName(string(repo)+":"+id)+".json")
}
func (s *FSStore) writeDocJob(j domain.DocFinalizationJob) error {
	if err := j.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return writeAtomic(s.docJobPath(j.RepoID, j.ID), b)
}
func (s *FSStore) readDocJob(repo domain.ContentHash, id string) (j domain.DocFinalizationJob, err error) {
	err = readJSON(s.docJobPath(repo, id), &j)
	if errors.Is(err, os.ErrNotExist) {
		err = domain.ErrNotFound
	}
	if err == nil {
		err = j.Validate()
	}
	return
}
func (s *FSStore) docJobsRaw() ([]domain.DocFinalizationJob, error) {
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "doc-jobs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var jobs []domain.DocFinalizationJob
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var j domain.DocFinalizationJob
		if err := readJSON(filepath.Join(s.dataDir, "doc-jobs", e.Name()), &j); err != nil {
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
func (s *FSStore) EnqueueDocJob(ctx context.Context, j domain.DocFinalizationJob) (domain.DocFinalizationJob, error) {
	if err := ctx.Err(); err != nil {
		return j, err
	}
	if err := j.Validate(); err != nil {
		return j, err
	}
	if j.State != "waiting" || j.Version != 0 || j.Attempts != 0 {
		return j, domain.ErrValidation
	}
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	old, err := s.readDocJob(j.RepoID, j.ID)
	if err == nil {
		if old.State != "completed" {
			return old, nil
		}
		have, err := s.HasDocs(ctx, j.RepoID, []domain.ContentHash{j.DocHash})
		if err != nil {
			return old, err
		}
		if len(have) > 0 {
			return old, nil
		}
		j.Version = old.Version + 1 // a previously collected body needs verification again
	} else if !errors.Is(err, domain.ErrNotFound) {
		return j, err
	}
	jobs, err := s.docJobsRaw()
	if err != nil {
		return j, err
	}
	count := 0
	for _, v := range jobs {
		if v.RepoID == j.RepoID && v.Pending() {
			count++
		}
	}
	if count >= domain.MaxPendingDocJobs {
		return j, domain.ErrConflict
	}
	return j, s.writeDocJob(j)
}
func (s *FSStore) GetDocJob(ctx context.Context, repo domain.ContentHash, id string) (domain.DocFinalizationJob, error) {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.DocFinalizationJob{}, err
	}
	return s.readDocJob(repo, id)
}
func (s *FSStore) ClaimDocJob(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.DocFinalizationJob, error) {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.DocFinalizationJob{}, err
	}
	jobs, err := s.docJobsRaw()
	if err != nil {
		return domain.DocFinalizationJob{}, err
	}
	running := map[domain.ContentHash]string{}
	for _, j := range jobs {
		if j.State == "running" {
			running[j.RepoID] = j.ID
		}
	}
	for _, j := range jobs {
		if (repo != "" && repo != j.RepoID) || (running[j.RepoID] != "" && running[j.RepoID] != j.ID) || !j.Due(now) {
			continue
		}
		j = j.Claim(now, lease)
		return j, s.writeDocJob(j)
	}
	return domain.DocFinalizationJob{}, domain.ErrNotFound
}
func (s *FSStore) RenewDocJob(ctx context.Context, j domain.DocFinalizationJob, now time.Time, lease time.Duration) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	old, err := s.readDocJob(j.RepoID, j.ID)
	if err != nil {
		return err
	}
	if !old.Fences(j, now) {
		return domain.ErrConflict
	}
	old.LeaseUntil = now.Add(lease)
	old.UpdatedAt = now
	return s.writeDocJob(old)
}
func (s *FSStore) FinishDocJob(ctx context.Context, j domain.DocFinalizationJob, now time.Time) error {
	if j.State != "retrying" && j.State != "rejected" {
		return domain.ErrValidation
	}
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	old, err := s.readDocJob(j.RepoID, j.ID)
	if err != nil {
		return err
	}
	if !old.Fences(j, now) {
		return domain.ErrConflict
	}
	return s.writeDocJob(j)
}

// Development only: serialization + idempotent recovery, not cross-file ACID.
// A crash after the body write leaves a reclaimable job; replay verifies/dedups it.
func (s *FSStore) CompleteDocJob(ctx context.Context, j domain.DocFinalizationJob, doc domain.VerifiedSessionDoc, now time.Time) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	old, err := s.readDocJob(j.RepoID, j.ID)
	if err != nil {
		return err
	}
	now = time.Now().UTC()
	if !old.Fences(j, now) {
		return domain.ErrConflict
	}
	if !doc.Valid() || doc.Hash() != old.DocHash {
		return domain.ErrIntegrity
	}
	if _, err := s.PutVerifiedDoc(ctx, j.RepoID, doc); err != nil {
		return err
	}
	old.State = "completed"
	old.Reason = ""
	old.UpdatedAt = now
	old.LeaseUntil = time.Time{}
	return s.writeDocJob(old)
}
