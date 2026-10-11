package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func repositoryReadForContribution(ctx context.Context, service *Service, checkpoint domain.PRHistoryCheckpoint, prepared *preparedPRParticipations) error {
	_, err := repositoryReadForRepo(ctx, service, checkpoint.RepoID, func(read context.Context) (struct{}, error) {
		revision, err := service.RepositoryRevision(read, checkpoint.RepoID)
		if err != nil {
			return struct{}{}, err
		}
		if revision.Graph != prepared.revision.Graph || revision.Evidence != prepared.revision.Evidence {
			return struct{}{}, domain.ErrConflict
		}
		prepared.contributions, prepared.memories, err = service.preparePRContributions(read, checkpoint, prepared.participations, prepared.history)
		return struct{}{}, err
	})
	return err
}

func (s *Service) applyPreparedPRContributions(ctx context.Context, checkpoint domain.PRHistoryCheckpoint, prepared preparedPRParticipations) error {
	if len(prepared.contributions) == 0 {
		return nil
	}
	ledger, ok := s.meta.(outbound.PRContributionStore)
	if !ok {
		return domain.ErrIntegrity
	}
	for _, contribution := range prepared.contributions {
		if err := ledger.PutPRContribution(ctx, contribution); err != nil {
			return err
		}
	}
	for _, memory := range prepared.memories {
		if err := ledger.PutPRContributionMemory(ctx, memory); err != nil {
			return err
		}
	}
	jobs, err := s.prJobs()
	if err != nil {
		return err
	}
	job, err := jobs.GetPRJob(ctx, checkpoint.RepoID, domain.PRPromotionID(checkpoint.RepoID, checkpoint.PR.Number))
	if err != nil {
		return err
	}
	if job.PR != checkpoint.PR || job.BaseBranchID != prepared.contributions[0].DestinationBranchID || job.GitOrigin != normalizeGitURL(checkpoint.GitOrigin) {
		return domain.ErrConflict
	}
	if job.State == "completed" || job.State == "running" {
		return nil
	}
	if job.State == "attention" && job.Reason != "source_finalization_required" {
		return nil
	}
	now := time.Now().UTC()
	if err := jobs.RetryPRJob(ctx, job.RepoID, job.ID, now); err != nil {
		return err
	}
	claimed, err := jobs.ClaimPRJob(ctx, job.RepoID, job.ID, now, prJobLease)
	if err != nil {
		return err
	}
	claimed.State, claimed.Reason, claimed.FailureClass = "completed", "", ""
	claimed.UpdatedAt, claimed.LeaseUntil = now, time.Time{}
	return jobs.FinishPRJob(ctx, claimed)
}
