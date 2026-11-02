package projectionworker

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Pending struct {
	scope   WorkScope
	jobs    outbound.PendingProjectionStore
	source  outbound.RepositoryViewSourceStore
	enabled bool
}

func NewPending(config PendingConfig) (*Pending, error) {
	if !validProfile(config.Profile) || !present(config.Scope) {
		return nil, domain.ErrValidation
	}
	for _, dependency := range []any{config.Jobs, config.Source} {
		if (dependency != nil || config.Profile == Complete) && !present(dependency) {
			return nil, domain.ErrValidation
		}
	}
	return &Pending{scope: config.Scope, jobs: config.Jobs, source: config.Source, enabled: config.Enabled}, nil
}

func (worker *Pending) Process(ctx context.Context, limit int) error {
	if limit <= 0 || limit > 128 {
		return domain.ErrValidation
	}
	if worker.jobs == nil {
		return nil
	}
	if worker.source == nil {
		return domain.ErrCatalogUnsupported
	}
	ctx = worker.scope.WorkerContext(ctx)
	for processed := 0; processed < limit; processed++ {
		claimed := false
		err := worker.scope.WithinWork(ctx, func(bound context.Context) error {
			job, err := func() (domain.PendingProjectionLease, error) {
				claim, cancel := context.WithTimeout(bound, 30*time.Second)
				defer cancel()
				return worker.jobs.ClaimPendingProjection(claim, time.Minute)
			}()
			if err != nil {
				return err
			}
			claimed = true
			return worker.RunLease(bound, job)
		})
		if !claimed && errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (worker *Pending) RunLease(ctx context.Context, job domain.PendingProjectionLease) error {
	work, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := worker.scope.CheckRepository(work, job.RepoID)
	if err == nil {
		switch job.Phase {
		case "capture":
			var page domain.RepositoryViewSourcePage
			page, err = worker.source.RepositoryViewSourceChanges(work, job.RepoID, job.SourceRequest())
			if err == nil {
				err = worker.jobs.SavePendingProjectionPage(work, job, page)
			}
		case "project":
			var entries []domain.RepositoryViewSourceEntry
			entries, err = worker.jobs.LoadPendingProjectionInputs(work, job)
			if err == nil {
				var view domain.PendingStateView
				view, err = domain.ProjectPendingSource(work, job.RepoID, entries)
				if err == nil {
					var parts []domain.PendingProjectionPart
					parts, err = domain.EncodePendingProjection(work, job.RepoID, view)
					if err == nil {
						err = worker.jobs.PublishPendingProjection(work, job, parts)
					}
				}
			}
		default:
			err = domain.ErrIntegrity
		}
	}
	if err == nil || ctx.Err() != nil {
		return err
	}

	reason, retry := "temporary_failure", true
	switch {
	case errors.Is(err, domain.ErrCatalogResetRequired):
		reason = "source_reset"
	case errors.Is(err, domain.ErrRefConflict), errors.Is(err, domain.ErrConflict):
		reason = "lease_lost"
	case errors.Is(err, domain.ErrRepositoryViewSourceLimit):
		reason, retry = "resource_limit", false
	case errors.Is(err, domain.ErrIntegrity), errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrNotFound):
		reason, retry = "invalid_source", false
	case errors.Is(err, domain.ErrForbidden):
		reason, retry = "access_denied", false
	case errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired):
		reason = "identity_upgrade_required"
	}
	finish, release := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer release()
	return errors.Join(err, worker.jobs.FailPendingProjection(finish, job, reason, retry))
}

func (worker *Pending) Run(ctx context.Context) {
	if !worker.enabled {
		return
	}
	for {
		if err := worker.Process(ctx, 16); err != nil && ctx.Err() == nil {
			log.Printf("pending projection: %v", err)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
