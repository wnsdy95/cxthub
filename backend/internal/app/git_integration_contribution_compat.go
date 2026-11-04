package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (scans *GitScans) prepareGitContribution(ctx context.Context, admission domain.GitContributionAdmission, head domain.GitContextHead) (preparedGitContribution, error) {
	return scans.integration().prepareGitContribution(ctx, admission, head)
}

func (scans *GitScans) applyGitContribution(ctx context.Context, prepared preparedGitContribution, authorization gitAdmissionAuthorization) (domain.GitContributionProof, error) {
	return scans.integration().applyGitContribution(ctx, prepared, authorization)
}

func (scans *GitScans) prepareGitContributionSupplement(ctx context.Context, job domain.GitContributionSupplement) (preparedGitContributionSupplement, error) {
	return scans.integration().prepareGitContributionSupplement(ctx, job)
}

func (scans *GitScans) applyGitContributionSupplement(ctx context.Context, prepared preparedGitContributionSupplement, authorization gitAdmissionAuthorization) error {
	return scans.integration().applyGitContributionSupplement(ctx, prepared, authorization)
}

func (scans *GitScans) ProcessContributions(ctx context.Context, limit int) error {
	return scans.integration().ProcessContributions(ctx, limit)
}

func (scans *GitScans) runGitContribution(ctx context.Context, job domain.GitContributionWork) error {
	return scans.integration().runGitContribution(ctx, job)
}

func (scans *GitScans) processGitContributionSupplements(ctx context.Context, repo domain.ContentHash, limit int) error {
	return scans.integration().processGitContributionSupplements(ctx, repo, limit)
}

func (scans *GitScans) runGitContributionSupplement(ctx context.Context, job domain.GitContributionSupplement) error {
	return scans.integration().runGitContributionSupplement(ctx, job)
}
