package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type contextContributionProjection struct {
	record            contributionMemoryRecord
	valid             bool
	publications      map[string]domain.HistoryEvent
	sourceBranch      string
	destinationBranch string
	prNumber          int
	mergedAt          time.Time
}

func (s *Service) contextContributionsForEvidence(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent, evidence *codeEvidence) ([]contextContributionProjection, error) {
	prs, err := s.contributionsForEvidence(ctx, repo, history, evidence)
	if err != nil {
		return nil, err
	}
	git, err := s.gitContributionsForEvidence(ctx, repo, history, evidence)
	if err != nil {
		return nil, err
	}
	result := make([]contextContributionProjection, 0, len(prs)+len(git))
	for _, verified := range prs {
		record := verified.Record
		result = append(result, contextContributionProjection{record: contributionMemoryRecord{ID: record.ID, RepoID: record.RepoID, DestinationBranchID: record.DestinationBranchID, SourceBranchID: record.SourceBranchID, Capture: record.Capture, OriginalHead: record.OriginalHead, IntegrationSHA: record.PR.MergeSHA}, valid: verified.Valid, publications: verified.Publications, sourceBranch: record.PR.HeadBranch, destinationBranch: record.PR.BaseBranch, prNumber: record.PR.Number, mergedAt: verified.MergedAt})
	}
	for _, verified := range git {
		record := verified.Record
		result = append(result, contextContributionProjection{record: contributionMemoryRecord{ID: record.ID, RepoID: record.RepoID, DestinationBranchID: record.DestinationBranchID, SourceBranchID: record.SourceBranchID, Capture: record.Capture, OriginalHead: record.OriginalHead, IntegrationSHA: record.IntegrationSHA}, valid: verified.Valid, publications: verified.Publications, sourceBranch: verified.SourceBranch, destinationBranch: verified.DestinationBranch})
	}
	return result, nil
}

func (s *Service) contributionsForEvidence(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent, evidence *codeEvidence) ([]verifiedPRContribution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := evidence.bindContributionHistory(repo, history); err != nil {
		return nil, err
	}
	if evidence != nil && evidence.contributionsLoaded {
		return evidence.contributions, nil
	}
	rows, err := s.verifiedPRContributions(ctx, repo, history)
	if err == nil && evidence != nil {
		evidence.contributions, evidence.contributionsLoaded = rows, true
	}
	return rows, err
}

func contributionMergeKey(scope string, source domain.ContentHash, merge string) string {
	return contextprojection.ContributionMergeKey(scope, source, merge)
}

func appendContributionOperations(graph *domain.GraphState, records []contextContributionProjection) {
	contextprojection.AppendContributionOperations(graph, branchContributionFacts(records))
}
