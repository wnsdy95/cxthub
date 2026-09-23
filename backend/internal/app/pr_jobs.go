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
	st, ok := s.meta.(outbound.PRJobStore)
	if !ok {
		return nil, fmt.Errorf("durable PR queue unavailable")
	}
	return st, nil
}
func (s *Service) submitPRPromotion(ctx context.Context, repo domain.ContentHash, pr domain.PullRequestMerge) (domain.PRPromotionJob, error) {
	if err := pr.Validate(); err != nil {
		return domain.PRPromotionJob{}, fmt.Errorf("%w: %v", domain.ErrValidation, err)
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.PRPromotionJob{}, err
	}
	metadata, err := s.meta.GetRepo(ctx, repo)
	if err != nil {
		return domain.PRPromotionJob{}, err
	}
	if metadata.GitRemoteURL == "" {
		return domain.PRPromotionJob{}, fmt.Errorf("%w: repository has no Git origin", domain.ErrValidation)
	}
	st, err := s.prJobs()
	if err != nil {
		return domain.PRPromotionJob{}, err
	}
	existing, err := st.GetPRJob(ctx, repo, domain.PRPromotionID(repo, pr.Number))
	if err == nil {
		if existing.PR != pr || existing.GitOrigin != normalizeGitURL(metadata.GitRemoteURL) {
			return existing, domain.ErrConflict
		}
		if e := s.queuePRGitScans(ctx, repo, metadata.GitRemoteURL, pr); e != nil {
			return existing, e
		}
		return existing, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return domain.PRPromotionJob{}, err
	}
	events, err := s.ListHistory(ctx, repo)
	if err != nil {
		return domain.PRPromotionJob{}, err
	}
	bindings, err := domain.ProjectContextBranches(events)
	if err != nil {
		return domain.PRPromotionJob{}, err
	}
	baseID := domain.LegacyContextBranchID(string(repo), pr.BaseBranch)
	if b, ok := bindings.Active[pr.BaseBranch]; ok {
		baseID = b.ID
	} else if bindings.Released[pr.BaseBranch] != "" {
		return domain.PRPromotionJob{}, fmt.Errorf("%w: PR base was released", domain.ErrConflict)
	}
	if err := s.queuePRGitScans(ctx, repo, metadata.GitRemoteURL, pr); err != nil {
		return domain.PRPromotionJob{}, err
	}
	now := time.Now().UTC()
	return st.EnqueuePRJob(ctx, domain.PRPromotionJob{ID: domain.PRPromotionID(repo, pr.Number), RepoID: repo, PR: pr, BaseBranchID: baseID, GitOrigin: normalizeGitURL(metadata.GitRemoteURL), State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now})
}
func (s *Service) ListPRPromotions(ctx context.Context, repo domain.ContentHash) ([]domain.PRPromotionJob, error) {
	st, err := s.prJobs()
	if err != nil {
		return nil, err
	}
	return st.ListPRJobs(ctx, repo)
}
func (s *Service) retryPRPromotionCommand(ctx context.Context, repo domain.ContentHash, id string) error {
	st, err := s.prJobs()
	if err != nil {
		return err
	}
	return st.RetryPRJob(ctx, repo, id, time.Now().UTC())
}

// Compatibility path: acceptance survives caller cancellation and synchronous failure.
func (s *Service) DeliverPRPromotion(ctx context.Context, repo domain.ContentHash, pr domain.PullRequestMerge) (inbound.UpdateRefOutput, error) {
	j, err := s.SubmitPRPromotion(ctx, repo, pr)
	if err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	if j.State == "completed" {
		return s.promoteBoundPR(ctx, repo, pr, j.BaseBranchID)
	}
	st, _ := s.prJobs()
	j, err = st.ClaimPRJob(ctx, repo, j.ID, time.Now().UTC(), prJobLease)
	if errors.Is(err, domain.ErrNotFound) {
		return inbound.UpdateRefOutput{}, fmt.Errorf("%w: PR promotion is durably queued", domain.ErrConflict)
	}
	if err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	return s.runPRJob(ctx, j)
}
func (s *Service) runPRJob(ctx context.Context, j domain.PRPromotionJob) (inbound.UpdateRefOutput, error) {
	work, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var out inbound.UpdateRefOutput
	var runErr error
	runErr = repositoryWriteError(work, s, j.RepoID, func(work context.Context) error {
		runErr = nil // a retry after a database abort starts from fresh state
		if fence, ok := s.meta.(outbound.PRJobFence); ok {
			if err := fence.FencePRJob(work, j); err != nil {
				return err
			}
		}
		repo, err := s.meta.GetRepo(work, j.RepoID)
		if err != nil {
			runErr = err
		} else if normalizeGitURL(repo.GitRemoteURL) != j.GitOrigin {
			runErr = fmt.Errorf("%w: repository origin changed", domain.ErrConflict)
		} else {
			if repo.RepositoryID != "" && s.repositories != nil {
				repository, err := s.repositories.GetRepository(work, repo.RepositoryID)
				if err != nil {
					runErr = err
				} else if repository.Archived {
					runErr = domain.ErrForbidden
				}
			}
			if runErr == nil {
				out, runErr = s.promoteBoundPR(work, j.RepoID, j.PR, j.BaseBranchID)
			}
		}
		if runErr != nil {
			return runErr
		}
		completed := j
		completed.State, completed.Reason = "completed", ""
		completed.UpdatedAt, completed.LeaseUntil = time.Now().UTC(), time.Time{}
		st, _ := s.prJobs()
		return st.FinishPRJob(work, completed)
	})
	if runErr == nil {
		return out, nil
	}
	now := time.Now().UTC()
	j.UpdatedAt = now
	j.LeaseUntil = time.Time{}
	j.Reason = ""
	switch {
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
	st, _ := s.prJobs()
	if err := st.FinishPRJob(finishCtx, j); err != nil {
		return out, errors.Join(runErr, err)
	}
	return out, runErr
}

// ProcessPRPromotions is bounded per tick, safe to run on multiple instances.
func (s *Service) ProcessPRPromotions(ctx context.Context, limit int) error {
	ctx = inbound.WithSystemActor(ctx)
	st, err := s.prJobs()
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
		j, err := st.ClaimPRJob(ctx, "", "", time.Now().UTC(), prJobLease)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = s.runPRJob(ctx, j)
		if err != nil {
			log.Printf("PR promotion %s attempt %d deferred: %v", j.ID, j.Attempts, err)
		}
	}
	return nil
}
func (s *Service) RunPRPromotionWorker(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := s.ProcessPRPromotions(ctx, 8); err != nil && !errors.Is(err, context.Canceled) {
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
	return repositoryWriteError(context.WithValue(ctx, revisionScopeKey{}, "none"), s, repo, func(ctx context.Context) error { return s.retryPRPromotionCommand(ctx, repo, id) })
}
