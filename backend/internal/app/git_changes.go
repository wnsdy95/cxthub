package app

import (
	"context"
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
	*gitChangeQuery
	sourceAccess GitSourceAuthorizer
	core         *Service
	reader       outbound.GitEvidenceReader
}

var _ inbound.GitChanges = (*GitChanges)(nil)

func NewGitChanges(core *Service, reader outbound.GitEvidenceReader) (*GitChanges, error) {
	st, ok := core.meta.(outbound.GitChangeStore)
	if !ok || reader == nil {
		return nil, fmt.Errorf("durable Git change verification unavailable")
	}
	return &GitChanges{core: core, gitChangeQuery: &gitChangeQuery{store: st, core: core}, reader: reader}, nil
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
func (g *GitChanges) Retry(ctx context.Context, repo domain.ContentHash, id string) error {
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	if err := domain.ValidateGitChangeID(id); err != nil {
		return err
	}
	return repositoryWriteError(evidenceWriteContext(ctx), g.core, repo, func(ctx context.Context) error { return g.store.RetryGitChange(ctx, repo, id, time.Now().UTC()) })
}
func (changes *GitChanges) run(ctx context.Context, job domain.GitChangeJob) error {
	worker, err := changes.worker()
	if err != nil {
		return err
	}
	return worker.RunLease(ctx, job)
}

func (changes *GitChanges) Process(ctx context.Context, limit int) error {
	worker, err := changes.worker()
	if err != nil {
		return err
	}
	return worker.Process(ctx, limit)
}

func (changes *GitChanges) Run(ctx context.Context) {
	worker, err := changes.worker()
	if err != nil {
		log.Printf("Git change worker: %v", err)
		return
	}
	worker.Run(ctx)
}

// SetSourceAuthorizer configures source policy before workers start.
func (g *GitChanges) SetSourceAuthorizer(a GitSourceAuthorizer) { g.sourceAccess = a }
