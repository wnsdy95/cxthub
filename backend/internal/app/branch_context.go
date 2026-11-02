package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (service *Service) branchContext(ctx context.Context, ref domain.Ref, code string, snapshots []domain.Snapshot, history []domain.HistoryEvent, evidence *codeEvidence) (domain.BranchContext, error) {
	inputs, err := newBranchQueryInputs(ctx, ref.RepoID, snapshots, history)
	if err != nil {
		return domain.BranchContext{}, err
	}
	return service.branchContextFromInputs(ctx, ref, code, inputs, evidence)
}

func (service *Service) branchContextFromInputs(ctx context.Context, ref domain.Ref, code string, inputs *branchQueryInputs, evidence *codeEvidence) (domain.BranchContext, error) {
	plan, err := contextprojection.PrepareBranchContext(ctx, ref, code, inputs, branchPolicyEvidence(evidence))
	if err != nil {
		return plan.Result(), err
	}
	contributions, err := service.contextContributionsForEvidence(ctx, ref.RepoID, inputs.History().Rows(), evidence)
	if err != nil {
		return plan.Result(), err
	}
	return plan.Finish(ctx, branchContributionFacts(contributions), branchPolicyEvidence(evidence))
}

func (s *Service) projectBranchMemory(ctx context.Context, repo domain.ContentHash, inclusion domain.BranchContext, snapshots []domain.Snapshot) (domain.MemoryProjection, error) {
	history, err := s.memoryHistory(ctx, repo)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	contributions, err := s.contextContributionsForEvidence(ctx, repo, history, nil)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	return s.projectBranchMemoryWithContributions(ctx, repo, inclusion, snapshots, contributions)
}

func (s *Service) projectBranchMemoryWithContributions(ctx context.Context, repo domain.ContentHash, inclusion domain.BranchContext, snapshots []domain.Snapshot, contributions []contextContributionProjection) (domain.MemoryProjection, error) {
	plan, err := s.prepareContextContributionMemory(ctx, repo, inclusion, snapshots, contributions)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	return memoryquery.ProjectBranchWithPlan(ctx, s.memoryProjectionReader(), repo, inclusion, plan)
}

type branchProjectionSource struct {
	MemoryProjectionSource
	rows []domain.Snapshot
}

func (s branchProjectionSource) ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error) {
	return s.rows, nil
}
