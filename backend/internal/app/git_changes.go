package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"log"
	"time"
)

// GitChanges owns evidence delivery. Provider I/O is never performed while
// holding a repository transaction. It cannot move refs or remove raw history.
type GitChanges struct {
	sourceAccess GitSourceAuthorizer
	core         *Service
	store        outbound.GitChangeStore
	reader       outbound.GitEvidenceReader
}

var _ inbound.GitChanges = (*GitChanges)(nil)

func NewGitChanges(core *Service, reader outbound.GitEvidenceReader) (*GitChanges, error) {
	st, ok := core.meta.(outbound.GitChangeStore)
	if !ok || reader == nil {
		return nil, fmt.Errorf("durable Git change verification unavailable")
	}
	return &GitChanges{core: core, store: st, reader: reader}, nil
}
func (g *GitChanges) Submit(ctx context.Context, repo domain.ContentHash, r domain.GitChangeRequest) (domain.GitChangeJob, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.GitChangeJob{}, err
	}
	if err := r.Validate(); err != nil {
		return domain.GitChangeJob{}, err
	}
	return repositoryWrite(evidenceWriteContext(ctx), g.core, repo, func(ctx context.Context) (domain.GitChangeJob, error) {
		metadata, err := g.core.meta.GetRepo(ctx, repo)
		if err != nil {
			return domain.GitChangeJob{}, err
		}
		if metadata.GitRemoteURL == "" {
			return domain.GitChangeJob{}, fmt.Errorf("%w: repository has no Git origin", domain.ErrValidation)
		}
		now := time.Now().UTC()
		j := domain.GitChangeJob{RepoID: repo, GitOrigin: metadata.GitRemoteURL, Request: r, State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
		j.ID = domain.GitChangeID(repo, j.GitOrigin, r)
		return g.store.EnqueueGitChange(ctx, j)
	})
}
func (g *GitChanges) Get(ctx context.Context, repo domain.ContentHash, id string) (domain.GitChangeJob, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.GitChangeJob{}, err
	}
	if err := domain.ValidateGitChangeID(id); err != nil {
		return domain.GitChangeJob{}, err
	}
	return g.store.GetGitChange(ctx, repo, id)
}
func (g *GitChanges) List(ctx context.Context, repo domain.ContentHash, cursor string, limit int) (domain.GitChangePage, error) {
	out := domain.GitChangePage{Items: []domain.GitChangeSummary{}}
	if err := domain.ValidateContentHash(repo); err != nil {
		return out, err
	}
	if cursor != "" {
		if err := domain.ValidateGitChangeID(cursor); err != nil {
			return out, err
		}
	}
	if limit < 1 || limit > 100 {
		return out, domain.ErrValidation
	}
	jobs, err := g.store.ListGitChanges(ctx, repo, cursor, limit+1)
	if err != nil {
		return out, err
	}
	if len(jobs) > limit {
		out.NextCursor = jobs[limit-1].ID
		jobs = jobs[:limit]
	}
	out.Items = jobs
	return out, nil
}
func (g *GitChanges) Retry(ctx context.Context, repo domain.ContentHash, id string) error {
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	if err := domain.ValidateGitChangeID(id); err != nil {
		return err
	}
	return repositoryWriteError(evidenceWriteContext(ctx), g.core, repo, func(ctx context.Context) error { return g.store.RetryGitChange(ctx, repo, id, time.Now().UTC()) })
}
func (g *GitChanges) run(ctx context.Context, j domain.GitChangeJob) error {
	work, cancel := context.WithTimeout(outbound.WithGitRepository(ctx, j.RepoID), 90*time.Second)
	defer cancel()
	repo, err := g.core.meta.GetRepo(work, j.RepoID)
	if err == nil && repo.GitRemoteURL != j.GitOrigin {
		err = domain.ErrConflict
	}
	var fence GitReadFence
	if err == nil {
		fence, err = authorizeGitRead(work, g.sourceAccess, j.RepoID)
	}
	var proof domain.GitReversalEvidence
	if err == nil {
		reader := g.reader
		if st, ok := g.core.meta.(outbound.GitScanStore); ok {
			reader = cachedGitEvidence{st, j.RepoID, reader}
		}
		proof, err = verifyGitReversal(work, reader, j.GitOrigin, j.Request.Target, j.Request.TargetParent, j.Request.Commit, j.Request.Parent)
	}
	now := time.Now().UTC()
	next := j
	next.UpdatedAt = now
	next.LeaseUntil = time.Time{}
	if err == nil {
		next.State = "completed"
		next.Result = &proof
		next.Reason = proof.Reason
		err = repositoryWriteError(evidenceWriteContext(work), g.core, j.RepoID, func(tx context.Context) error {
			current, e := g.core.meta.GetRepo(tx, j.RepoID)
			if e != nil {
				return e
			}
			if current.GitRemoteURL != j.GitOrigin {
				return domain.ErrConflict
			}
			// CAS and result publication are one store transition. A lost commit ack is
			// safe: retries see the terminal row and cannot overwrite its evidence.
			if e := fence(tx); e != nil {
				return e
			}
			return g.store.FinishGitChange(tx, next)
		})
		if err == nil {
			return nil
		}
	}
	next.Result = nil
	next.State = "retrying"
	next.Reason = "temporary_provider_or_storage_failure"
	switch {
	case errors.Is(err, domain.ErrIntegrity):
		next.State = "attention"
		next.Reason = "integrity_check_failed"
	case errors.Is(err, domain.ErrValidation):
		next.State = "attention"
		next.Reason = "invalid_or_ambiguous_git_evidence"
	case errors.Is(err, domain.ErrConflict):
		next.State = "attention"
		next.Reason = "origin_or_lease_changed"
	case errors.Is(err, domain.ErrNotFound):
		next.State = "attention"
		next.Reason = "repository_missing"
	}
	if next.State == "retrying" && next.Attempts >= 20 {
		next.State = "attention"
		next.Reason = "retry_limit_reached"
	}
	next.NextAttempt = now.Add(time.Second << min(next.Attempts, 8))
	finish, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	// If another worker completed, or our own COMMIT succeeded with a lost ack,
	// this fence rejects the stale failure state without changing the result.
	e := repositoryWriteError(evidenceWriteContext(finish), g.core, j.RepoID, func(tx context.Context) error { return g.store.FinishGitChange(tx, next) })
	return errors.Join(err, e)
}
func (g *GitChanges) Process(ctx context.Context, limit int) error {
	ctx = inbound.WithSystemActor(ctx)
	for i := 0; i < limit; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		j, err := g.store.ClaimGitChange(ctx, "", "", time.Now().UTC(), 2*time.Minute)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = g.run(ctx, j); err != nil {
			log.Printf("Git change verification %s attempt %d deferred", j.ID, j.Attempts)
		}
	}
	return nil
}
func (g *GitChanges) Run(ctx context.Context) {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		if err := g.Process(ctx, 4); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Git change worker: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// SetSourceAuthorizer configures source policy before workers start.
func (g *GitChanges) SetSourceAuthorizer(a GitSourceAuthorizer) { g.sourceAccess = a }
