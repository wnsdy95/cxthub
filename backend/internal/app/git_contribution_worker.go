package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (scans *GitScans) ProcessContributions(ctx context.Context, limit int) error {
	if limit < 1 || limit > 8 {
		return domain.ErrValidation
	}
	store, supported := scans.core.meta.(outbound.GitContributionWorkStore)
	_, heads := scans.core.meta.(outbound.GitContextHeadStore)
	_, admissions := scans.core.meta.(outbound.GitContributionAdmissionStore)
	if !supported || !heads || !admissions {
		return nil
	}
	ctx = scans.core.workerDocumentIdentityContext(inbound.WithSystemActor(ctx))
	repos, err := scans.core.meta.ListRepos(ctx, "default")
	if err != nil {
		return err
	}
	var failures []error
	for _, repo := range repos {
		if repo.GitRemoteURL == "" {
			continue
		}
		for count := 0; count < limit; count++ {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failures, err)...)
			}
			job, err := store.ClaimGitContributionWork(ctx, repo.ID, time.Now().UTC(), time.Minute)
			if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || errors.Is(err, domain.ErrForbidden) {
				break
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("Git contribution claim %s: %w", repo.ID, err))
				break
			}
			if err := scans.runGitContribution(ctx, job); err != nil {
				failures = append(failures, fmt.Errorf("Git contribution %s: %w", job.AdmissionID, err))
			}
		}
		if err := scans.processGitContributionSupplements(ctx, repo.ID, limit); err != nil {
			failures = append(failures, fmt.Errorf("Git memory supplements %s: %w", repo.ID, err))
		}
	}
	return errors.Join(failures...)
}

func (scans *GitScans) runGitContribution(ctx context.Context, job domain.GitContributionWork) error {
	work, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	store := scans.core.meta.(outbound.GitContributionWorkStore)
	headStore := scans.core.meta.(outbound.GitContextHeadStore)
	admissions := scans.core.meta.(outbound.GitContributionAdmissionStore)
	next := job
	next.State, next.Reason, next.LeaseUntil = "waiting", "", time.Time{}
	next.NextAttempt = time.Now().UTC().Add(time.Second)
	var prepared *preparedGitContribution
	authorization, err := authorizeGitAdmission(work, scans.sourceAccess, job.RepoID)
	var admission domain.GitContributionAdmission
	if err == nil {
		admission, err = admissions.GetGitContributionAdmission(work, job.RepoID, job.AdmissionID)
	}
	if err == nil && (admission.AfterReadVersion != job.AfterReadVersion || admission.Destination.ProviderRepositoryID != authorization.provider.RepositoryID || admission.Destination.InstallationID != authorization.provider.InstallationID) {
		err = domain.ErrConflict
	}
	if err == nil {
		_, err = headStore.FindGitContributionProof(work, job.RepoID, job.AdmissionID)
		if err == nil {
			next.State = "completed"
		} else if errors.Is(err, domain.ErrNotFound) {
			err = nil
		}
	}
	if err == nil && next.State != "completed" {
		var heads []domain.GitContextHead
		heads, err = headStore.ListGitContextHeads(work, job.RepoID, admission.Destination.GitOrigin, admission.Destination.DestinationBranchID, job.ReadCursor, 1)
		if err == nil && (job.ReadThrough == 0 || len(heads) == 0 || heads[0].Destination.ReadVersion > job.ReadThrough) {
			next.ReadCursor, next.ReadThrough = job.AfterReadVersion, 0
			next.Reason = "awaiting_bound_head"
			next.NextAttempt = time.Now().UTC().Add(time.Minute)
		} else if err == nil {
			head := heads[0]
			next.ReadCursor = head.Destination.ReadVersion
			if head.Destination.InstallationID != admission.Destination.InstallationID || head.Destination.ProviderRepositoryID != admission.Destination.ProviderRepositoryID {
				next.Reason = "provider_scope_changed"
			} else {
				var result preparedGitContribution
				result, err = scans.prepareGitContribution(work, admission, head)
				if err == nil {
					prepared = &result
					next.State = "completed"
				}
			}
		}
	}
	if err == nil {
		err = repositoryWriteError(context.WithValue(work, revisionScopeKey{}, "none"), scans.core, job.RepoID, func(write context.Context) error {
			if err := authorization.fence(write); err != nil {
				return err
			}
			current, err := scans.core.workerRepository(write, job.RepoID)
			if err != nil {
				return err
			}
			if current.GitRemoteURL != admission.Destination.GitOrigin {
				return domain.ErrConflict
			}
			if prepared != nil {
				if _, err := scans.applyGitContribution(write, *prepared, authorization); err != nil {
					return err
				}
			}
			return store.FinishGitContributionWork(write, next)
		})
		if err == nil {
			return nil
		}
	}
	failure := gitContributionFailure(job, next, err, time.Now().UTC())
	finish, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	finishErr := repositoryWriteError(context.WithValue(finish, revisionScopeKey{}, "none"), scans.core, job.RepoID, func(write context.Context) error { return store.FinishGitContributionWork(write, failure) })
	if finishErr != nil {
		return errors.Join(err, finishErr)
	}
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, errGitContributionEvidencePending) || errors.Is(err, errGitContributionAlreadyPresent) {
		return nil
	}
	return err
}

func gitContributionFailure(job, attempted domain.GitContributionWork, err error, now time.Time) domain.GitContributionWork {
	failed := job
	failed.State, failed.Reason, failed.LeaseUntil = "retrying", "temporary_failure", time.Time{}
	failed.NextAttempt = now.Add(time.Second << min(job.Attempts, 9))
	switch {
	case errors.Is(err, domain.ErrIntegrity), errors.Is(err, domain.ErrValidation):
		failed.State, failed.Reason = "attention", "integrity_check_failed"
	case errors.Is(err, errGitContributionEvidenceLimit):
		failed.State, failed.Reason = "attention", "evidence_limit"
	case errors.Is(err, domain.ErrForbidden), errors.Is(err, domain.ErrConflict), errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired):
		failed.Reason = "authorization_or_state_changed"
	case errors.Is(err, errGitContributionEvidencePending), errors.Is(err, domain.ErrNotFound), errors.Is(err, errGitContributionAlreadyPresent):
		failed.State, failed.Reason, failed.ReadCursor = "waiting", "evidence_pending", attempted.ReadCursor
		failed.NextAttempt = now.Add(time.Second)
		switch {
		case errors.Is(err, errGitContributionRangeDiscontinuous):
			failed.Reason = "range_discontinuous"
		case errors.Is(err, errGitContributionAlreadyPresent):
			failed.Reason = "already_present"
		case errors.Is(err, domain.ErrNotFound):
			failed.Reason = "source_absent"
		}
	}
	return failed
}
