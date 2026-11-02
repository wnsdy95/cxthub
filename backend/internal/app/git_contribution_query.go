package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *Service) gitContributionsForEvidence(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent, evidence *codeEvidence) ([]verifiedGitContribution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := evidence.bindContributionHistory(repo, history); err != nil {
		return nil, err
	}
	if evidence != nil && evidence.gitContributionsLoaded {
		return evidence.gitContributions, nil
	}
	rows, err := s.queryGitContributions(ctx, repo, history)
	if err == nil && evidence != nil {
		evidence.gitContributions, evidence.gitContributionsLoaded = rows, true
	}
	return rows, err
}

func (service *Service) queryGitContributions(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) ([]verifiedGitContribution, error) {
	queries, err := service.evidenceQueries()
	if err != nil {
		return nil, err
	}
	return queries.VerifiedGit(ctx, repo, history)
}
