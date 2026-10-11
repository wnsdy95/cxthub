package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *Service) QueryMemoryPositions(ctx context.Context, repo, snapshot domain.ContentHash, event string) (domain.MemoryPositions, error) {
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(snapshot) != nil || len(event) > 128 {
		return domain.MemoryPositions{}, domain.ErrValidation
	}
	return repositoryReadForRepo(ctx, s, repo, func(ctx context.Context) (domain.MemoryPositions, error) {
		if _, err := s.meta.GetSnapshot(ctx, repo, snapshot); err != nil {
			return domain.MemoryPositions{}, err
		}
		history, err := s.ListHistory(ctx, repo)
		if err != nil {
			return domain.MemoryPositions{}, err
		}
		contributions, err := s.contextContributionsForEvidence(ctx, repo, history, nil)
		if err != nil {
			return domain.MemoryPositions{}, err
		}
		options := domain.ResolveMemoryPositions(snapshot, "", history).Options
		for _, contribution := range contributions {
			record := contribution.record
			if !contribution.valid || record.Capture != snapshot {
				continue
			}
			kind := "merge"
			if contribution.prNumber > 0 {
				kind = "pr-merge"
			}
			options = append(options, domain.MemoryCodePosition{EventID: record.ID, CodeCommit: record.IntegrationSHA, Branch: contribution.destinationBranch, Kind: kind, PRNumber: contribution.prNumber, CreatedAt: contribution.mergedAt})
		}
		return domain.ResolveMemoryPositionOptions(snapshot, event, options), nil
	})
}
