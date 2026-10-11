package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *Service) projectPRJobAssociations(ctx context.Context, repo domain.ContentHash, jobs []domain.PRPromotionJob) ([]domain.PRPromotionJob, error) {
	if len(jobs) == 0 {
		return jobs, ctx.Err()
	}
	var evidence outbound.PRJobAssociationEvidence
	storage, supported := s.meta.(outbound.PRJobAssociationStore)
	available := false
	if supported {
		numbers := make([]int, 0, len(jobs))
		for _, job := range jobs {
			numbers = append(numbers, job.PR.Number)
		}
		var err error
		evidence, err = storage.ReadPRJobAssociationEvidence(ctx, repo, numbers)
		available = err == nil
	}
	checkpoints := map[int][]domain.PRHistoryCheckpoint{}
	participations := map[int][]domain.PRParticipation{}
	contributions := map[int][]domain.PRContribution{}
	if available {
		for _, record := range evidence.Checkpoints {
			checkpoints[record.PR.Number] = append(checkpoints[record.PR.Number], record)
		}
		for _, record := range evidence.Participations {
			participations[record.Number] = append(participations[record.Number], record)
		}
		for _, record := range evidence.Contributions {
			contributions[record.PR.Number] = append(contributions[record.PR.Number], record)
		}
	}
	result := append([]domain.PRPromotionJob(nil), jobs...)
	for index := range result {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		job := &result[index]
		status := &domain.PRJobAssociation{
			ProviderHistory: "unavailable", Participation: "unavailable",
			Contribution: "unavailable", CurrentApplicability: "not_evaluated",
		}
		job.Association = status
		if !available {
			continue
		}
		status.ProviderHistory, status.Participation, status.Contribution = "unknown", "unknown", "unknown"
		if job.GitOrigin == "" {
			continue
		}
		if evidence.CheckpointsComplete {
			count, complete := 0, true
			for _, checkpoint := range checkpoints[job.PR.Number] {
				if checkpoint.RepoID != repo || normalizeGitURL(checkpoint.GitOrigin) != job.GitOrigin || checkpoint.PR != job.PR {
					continue
				}
				count++
				complete = complete && checkpoint.Complete
			}
			status.ProviderCheckpointCount = &count
			if count > 0 {
				status.ProviderHistory = "recorded_partial"
				if complete {
					status.ProviderHistory = "recorded_complete"
				}
			}
		}
		if evidence.ParticipationsComplete {
			ids, publications := map[string]bool{}, map[string]bool{}
			for _, proof := range participations[job.PR.Number] {
				if proof.RepoID == repo && normalizeGitURL(proof.GitOrigin) == job.GitOrigin && proof.Number == job.PR.Number && proof.MergeSHA == job.PR.MergeSHA {
					ids[proof.ID], publications[proof.PublicationID] = true, true
				}
			}
			count, publicationCount := len(ids), len(publications)
			status.Participation, status.ParticipationCount, status.PublicationCount = "not_recorded", &count, &publicationCount
			if count > 0 {
				status.Participation = "recorded"
			}
		}
		if evidence.ContributionsComplete && job.BaseBranchID != "" {
			ids := map[string]bool{}
			for _, record := range contributions[job.PR.Number] {
				if record.RepoID == repo && normalizeGitURL(record.GitOrigin) == job.GitOrigin && record.PR == job.PR && record.DestinationBranchID == job.BaseBranchID {
					ids[record.ID] = true
				}
			}
			count := len(ids)
			status.Contribution, status.ContributionCount = "not_recorded", &count
			if count > 0 {
				status.Contribution = "recorded"
			}
		}
	}
	return result, ctx.Err()
}
