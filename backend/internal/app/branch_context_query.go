package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func branchCode(ref domain.Ref, history []domain.HistoryEvent) string {
	return contextprojection.BranchCode(ref, history)
}

func branchCodeCandidates(ref domain.Ref, history []domain.HistoryEvent) map[string]bool {
	return contextprojection.BranchCodeCandidates(ref, history)
}

func resolvedBranchCode(ctx context.Context, ref domain.Ref, history []domain.HistoryEvent, evidence *codeEvidence) (string, error) {
	return contextprojection.ResolveBranchCode(ctx, ref, history, branchPolicyEvidence(evidence))
}

func resolvedBranchCodeFromIndex(ctx context.Context, ref domain.Ref, index *branchHistoryIndex, evidence *codeEvidence) (string, error) {
	return contextprojection.ResolveBranchCodeFromIndex(ctx, ref, index, branchPolicyEvidence(evidence))
}

func (s *Service) QueryBranchMemory(ctx context.Context, repo domain.ContentHash, branch string, snapshot domain.ContentHash, code string) (domain.MemoryProjection, error) {
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(snapshot) != nil || domain.ValidateBranchName(branch) != nil || (code != "" && domain.ValidateGitOID(code) != nil) {
		return domain.MemoryProjection{}, domain.ErrValidation
	}
	queries, err := s.LiveMemoryQueries()
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	return queries.QueryBranchMemory(ctx, repo, branch, snapshot, code)
}

func (service *Service) projectGraphState(ctx context.Context, view domain.RepositoryView, position string) (domain.GraphState, error) {
	if position != "" {
		return repositoryquery.ProjectGraphPosition(view, position)
	}
	projection, err := service.legacyGraphProjection()
	if err != nil {
		return domain.GraphState{}, err
	}
	return projection.Project(ctx, view, position)
}
