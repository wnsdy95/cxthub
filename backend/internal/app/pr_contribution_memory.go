package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type prContributionMemoryState = memoryquery.ContributionState

type prContributionMemoryVersion = memoryquery.ContributionVersion

type prContributionMemoryPinKey = memoryquery.ContributionPinKey

type prContributionMemoryPlan = memoryquery.ContributionPlan

const (
	prMemorySelected         = memoryquery.ContributionSelected
	prMemorySuperseded       = memoryquery.ContributionSuperseded
	prMemoryEmpty            = memoryquery.ContributionEmpty
	prMemoryUnavailable      = memoryquery.ContributionUnavailable
	prMemoryReview           = memoryquery.ContributionReview
	maxPRMemoryMetadataBytes = memoryquery.MaxContributionMetadataBytes
)

func (s *Service) preparePRContributionMemory(ctx context.Context, repo domain.ContentHash, inclusion domain.BranchContext, snapshots []domain.Snapshot, verified []verifiedPRContribution) (prContributionMemoryPlan, error) {
	var pins []domain.PRContributionMemory
	if len(verified) > 0 {
		ledger, ok := s.memoryPinReader().(outbound.PRContributionMemoryReader)
		if !ok {
			return prContributionMemoryPlan{}, domain.ErrIntegrity
		}
		var err error
		pins, err = ledger.ListPRContributionMemories(ctx, repo)
		if err != nil {
			return prContributionMemoryPlan{}, err
		}
	}
	return s.prepareVerifiedPRContributionMemory(ctx, repo, inclusion, snapshots, verified, pins)
}

func (s *Service) prepareVerifiedContributionMemory(ctx context.Context, repo domain.ContentHash, inclusion domain.BranchContext, snapshots []domain.Snapshot, verified []verifiedMemoryContribution, pins []contributionMemoryPin) (prContributionMemoryPlan, error) {
	return memoryquery.PrepareVerifiedContributionMemory(ctx, s.memoryProjectionReader(), repo, inclusion, snapshots, verified, pins)
}
