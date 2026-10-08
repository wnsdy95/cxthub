package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.DocJobStore = (*FSStore)(nil)

// FS mode has one writer process. Rebuild the scheduling index once from durable
// receipts after restart; completed manifests must not be reparsed on each poll.
// Share the index between handles, and isolate it from OAuth/runtime locks.
var fsDocQueues sync.Map

type fsDocQueue struct {
	sync.Mutex
	loaded  bool
	pending map[string]domain.DocFinalizationJob
}

func (s *FSStore) docQueue() *fsDocQueue {
	q, _ := fsDocQueues.LoadOrStore(s.dataDir, &fsDocQueue{})
	return q.(*fsDocQueue)
}
func (s *FSStore) pendingDocJobs() ([]domain.DocFinalizationJob, error) {
	q := s.docQueue() // caller holds this lock
	if !q.loaded {
		all, err := s.docJobsRaw()
		if err != nil {
			return nil, err
		}
		q.pending = make(map[string]domain.DocFinalizationJob)
		for _, j := range all {
			q.record(j)
		}
		q.loaded = true
	}
	jobs := make([]domain.DocFinalizationJob, 0, len(q.pending))
	for _, j := range q.pending {
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
func (q *fsDocQueue) record(j domain.DocFinalizationJob) {
	if !j.Pending() {
		delete(q.pending, j.ID)
		return
	}
	// Scheduling uses only metadata. The selected job is read and validated from
	// its durable receipt before execution; mutable manifest slices never escape.
	j.Manifest = domain.DocChunkManifest{}
	q.pending[j.ID] = j
}

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
	if err := writeAtomic(s.docJobPath(j.RepoID, j.ID), b); err != nil {
		return err
	}
	if q := s.docQueue(); q.loaded {
		q.record(j)
	}
	return nil
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
	l := s.docQueue()
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
	jobs, err := s.pendingDocJobs()
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
	if err := ctx.Err(); err != nil {
		return domain.DocFinalizationJob{}, err
	}
	return s.readDocJob(repo, id)
}
func (s *FSStore) ClaimDocJob(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.DocFinalizationJob, error) {
	l := s.docQueue()
	l.Lock()
	defer l.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.DocFinalizationJob{}, err
	}
	jobs, err := s.pendingDocJobs()
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
		persisted, err := s.readDocJob(j.RepoID, j.ID)
		if err != nil {
			return domain.DocFinalizationJob{}, err
		}
		j = persisted.Claim(now, lease)
		return j, s.writeDocJob(j)
	}
	return domain.DocFinalizationJob{}, domain.ErrNotFound
}
func (s *FSStore) RenewDocJob(ctx context.Context, j domain.DocFinalizationJob, now time.Time, lease time.Duration) error {
	l := s.docQueue()
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
	l := s.docQueue()
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
	if doc.DocumentRef().Identity != domain.DocumentIdentityLegacy {
		return domain.ErrUnsupportedDocumentIdentity
	}
	l := s.docQueue()
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

type fsDocPublication struct {
	store *FSStore
	doc   domain.VerifiedSessionDoc
}

func (s *FSStore) PrepareDocJob(ctx context.Context, doc domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error) {
	if doc.DocumentRef().Identity != domain.DocumentIdentityLegacy {
		return nil, domain.ErrUnsupportedDocumentIdentity
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !doc.Valid() {
		return nil, domain.ErrIntegrity
	}
	return fsDocPublication{s, doc}, nil
}

func (p fsDocPublication) Complete(ctx context.Context, j domain.DocFinalizationJob, now time.Time) error {
	return p.store.CompleteDocJob(ctx, j, p.doc, now)
}
