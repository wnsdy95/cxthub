package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (scans *GitScans) processGitContributionSupplements(ctx context.Context, repo domain.ContentHash, limit int) error {
	queue, supported := scans.core.meta.(outbound.GitContributionSupplementStore)
	if !supported {
		return nil
	}
	var failures []error
	for count := 0; count < limit; count++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		job, err := queue.ClaimGitContributionSupplement(ctx, repo, time.Now().UTC(), time.Minute)
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || errors.Is(err, domain.ErrForbidden) {
			break
		}
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := scans.runGitContributionSupplement(ctx, job); err != nil {
			failures = append(failures, fmt.Errorf("Git memory supplement %s: %w", job.ID, err))
		}
	}
	return errors.Join(failures...)
}

func (scans *GitScans) runGitContributionSupplement(ctx context.Context, job domain.GitContributionSupplement) error {
	work, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	authorization, err := authorizeGitAdmission(work, scans.sourceAccess, job.RepoID)
	var prepared preparedGitContributionSupplement
	if err == nil {
		prepared, err = scans.prepareGitContributionSupplement(work, job)
	}
	if err == nil {
		err = scans.applyGitContributionSupplement(work, prepared, authorization)
	}
	if err == nil {
		return nil
	}
	failure := gitContributionSupplementFailure(job, err, time.Now().UTC())
	finish, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	finishErr := repositoryWriteError(context.WithValue(finish, revisionScopeKey{}, "none"), scans.core, job.RepoID, func(write context.Context) error {
		return scans.core.meta.(outbound.GitContributionSupplementStore).FinishGitContributionSupplement(write, failure)
	})
	if finishErr != nil {
		return errors.Join(err, finishErr)
	}
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	return err
}

func gitContributionSupplementFailure(job domain.GitContributionSupplement, err error, now time.Time) domain.GitContributionSupplement {
	job.State, job.Reason, job.LeaseUntil = "retrying", "temporary_failure", time.Time{}
	job.NextAttempt = now.Add(time.Second << min(job.Attempts, 9))
	switch {
	case errors.Is(err, domain.ErrIntegrity), errors.Is(err, domain.ErrValidation):
		job.State, job.Reason = "attention", "integrity_check_failed"
	case errors.Is(err, domain.ErrMemoryProjectionLimit):
		job.State, job.Reason = "attention", "projection_limit"
	case errors.Is(err, domain.ErrNotFound):
		job.State, job.Reason = "waiting", "evidence_pending"
	case errors.Is(err, domain.ErrForbidden), errors.Is(err, domain.ErrConflict), errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired):
		job.Reason = "authorization_or_state_changed"
	}
	return job
}
