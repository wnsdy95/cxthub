package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
)

func branchPolicyEvidence(evidence *codeEvidence) contextprojection.CodeEvidence {
	if evidence == nil {
		return nil
	}
	return evidence
}

func (evidence *codeEvidence) Origin() string {
	return evidence.origin
}

func (evidence *codeEvidence) Relation(ctx context.Context, source, selected string) (string, error) {
	return evidence.relation(ctx, source, selected)
}

func (evidence *codeEvidence) ExhaustTraversal() error {
	return evidence.exhaustTraversal()
}

type branchContributionFacts []contextContributionProjection

func (facts branchContributionFacts) Len() int {
	return len(facts)
}

func (facts branchContributionFacts) At(index int) contextprojection.Contribution {
	verified := facts[index]
	return contextprojection.Contribution{
		ID: verified.record.ID, DestinationBranchID: verified.record.DestinationBranchID,
		Capture: verified.record.Capture, IntegrationSHA: verified.record.IntegrationSHA,
		Valid: verified.valid, SourceBranch: verified.sourceBranch, DestinationBranch: verified.destinationBranch,
		PRNumber: verified.prNumber, MergedAt: verified.mergedAt,
	}
}

var _ contextprojection.CodeEvidence = (*codeEvidence)(nil)
var _ contextprojection.Contributions = branchContributionFacts(nil)
