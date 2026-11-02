package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const prJobLease = 2 * time.Minute
const prSourcePendingAttempts = 8

func (s *Service) prJobs() (outbound.PRJobStore, error) {
	return prJobsFromMetadata(s.meta)
}
func (s *Service) submitPRPromotion(ctx context.Context, repo domain.ContentHash, pr domain.PullRequestMerge) (domain.PRPromotionJob, error) {
	return s.prPublications().submitPRPromotion(ctx, repo, pr)
}
func (s *Service) ListPRPromotions(ctx context.Context, repo domain.ContentHash) ([]domain.PRPromotionJob, error) {
	return s.prCoordinations().ListPRPromotions(ctx, repo)
}

// Compatibility path: acceptance survives caller cancellation and synchronous failure.
func (s *Service) DeliverPRPromotion(ctx context.Context, repo domain.ContentHash, pr domain.PullRequestMerge) (inbound.UpdateRefOutput, error) {
	return s.prCoordinations().DeliverPRPromotion(ctx, repo, pr)
}
func (worker *PRPromotionWorker) runPRJob(ctx context.Context, j domain.PRPromotionJob) (inbound.UpdateRefOutput, error) {
	work, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var out inbound.UpdateRefOutput
	var runErr error
	runErr = repositoryWriteErrorWithWork(work, worker.work, j.RepoID, func(work context.Context) error {
		runErr = nil // a retry after a database abort starts from fresh state
		if err := work.Err(); err != nil {
			return err
		}
		if fence, ok := worker.meta.(outbound.PRJobFence); ok {
			if err := fence.FencePRJob(work, j); err != nil {
				return err
			}
		}
		repo, err := worker.meta.GetRepo(work, j.RepoID)
		if err != nil {
			runErr = err
		} else if normalizeGitURL(repo.GitRemoteURL) != j.GitOrigin {
			runErr = fmt.Errorf("%w: repository origin changed", domain.ErrConflict)
		} else {
			if repo.RepositoryID != "" && worker.repositories != nil {
				repository, err := worker.repositories.GetRepository(work, repo.RepositoryID)
				if err != nil {
					runErr = err
				} else if repository.Archived {
					runErr = domain.ErrForbidden
				}
			}
			if runErr == nil {
				out, runErr = worker.promoteBoundPR(work, j.RepoID, j.PR, j.BaseBranchID)
			}
		}
		if runErr != nil {
			return runErr
		}
		completed := j
		completed.State, completed.Reason = "completed", ""
		completed.FailureClass = ""
		completed.UpdatedAt, completed.LeaseUntil = time.Now().UTC(), time.Time{}
		st, _ := worker.prJobs()
		return st.FinishPRJob(work, completed)
	})
	if runErr == nil {
		return out, nil
	}
	now := time.Now().UTC()
	j.UpdatedAt = now
	j.LeaseUntil = time.Time{}
	j.Reason = ""
	j.FailureClass = prJobFailureClass(runErr)
	switch {
	case errors.Is(runErr, domain.ErrPROrderingPending):
		j.State = "waiting"
		j.Reason = "ordering_evidence_pending"
	case errors.Is(runErr, domain.ErrPRSourcePending):
		j.State = "waiting"
		j.Reason = "source_context_pending"
		if j.Attempts >= prSourcePendingAttempts {
			j.State = "attention"
			j.Reason = "source_finalization_required"
		}
	case errors.Is(runErr, domain.ErrIntegrity):
		j.State = "attention"
		j.Reason = "integrity_check_failed"
	case errors.Is(runErr, domain.ErrForbidden):
		j.State = "attention"
		j.Reason = "policy_changed"
	case errors.Is(runErr, domain.ErrValidation):
		j.State = "attention"
		j.Reason = "invalid_request"
	case errors.Is(runErr, domain.ErrConflict) && !errors.Is(runErr, domain.ErrRefConflict):
		j.State = "attention"
		j.Reason = "identity_or_history_conflict"
	case errors.Is(runErr, domain.ErrNotFound):
		j.State = "attention"
		j.Reason = "repository_or_base_missing"
	default:
		j.State = "retrying"
		j.Reason = "temporary_failure"
		if j.Attempts >= 20 {
			j.State = "attention"
			j.Reason = "retry_limit_reached"
		}
	}
	delay := time.Second << min(j.Attempts, 8)
	j.NextAttempt = now.Add(delay)
	// A canceled request must still release its lease/persist retry state.
	finishCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	st, _ := worker.prJobs()
	if err := st.FinishPRJob(finishCtx, j); err != nil {
		return out, errors.Join(runErr, err)
	}
	return out, runErr
}

// Only bounded classifications cross the read boundary; raw errors may contain
// upstream URLs, credentials, or provider content.
func prJobFailureClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "unspecified"
	}
}

// ProcessPRPromotions is bounded per tick, safe to run on multiple instances.
func (worker *PRPromotionWorker) ProcessPRPromotions(ctx context.Context, limit int) error {
	ctx = worker.workerDocumentIdentityContext(inbound.WithSystemActor(ctx))
	st, err := worker.prJobs()
	if err != nil {
		return err
	}
	// Reconcile persisted witnesses after a crash or a publication racing the
	// worker's transition into attention. No new delivery is required to wake it.
	if err := st.WakePRSourceJobs(ctx, "", time.Now().UTC()); err != nil {
		return err
	}
	for i := 0; i < limit; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := admittedJob(ctx, func(bound context.Context) (domain.PRPromotionJob, error) {
			return st.ClaimPRJob(bound, "", "", time.Now().UTC(), prJobLease)
		}, func(bound context.Context, job domain.PRPromotionJob) error {
			if _, err := worker.runPRJob(bound, job); err != nil {
				log.Printf("PR promotion %s attempt %d deferred: %v", job.ID, job.Attempts, err)
			}
			return nil
		})
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func (worker *PRPromotionWorker) RunPRPromotionWorker(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := worker.ProcessPRPromotions(ctx, 8); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("PR promotion worker: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) RetryPRPromotion(ctx context.Context, repo domain.ContentHash, id string) error {
	return s.prCoordinations().RetryPRPromotion(ctx, repo, id)
}
