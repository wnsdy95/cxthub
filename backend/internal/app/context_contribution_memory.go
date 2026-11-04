package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type contributionMemoryRecord = memoryquery.ContributionRecord

type verifiedMemoryContribution = memoryquery.VerifiedContribution

type verifiedGitContribution = evidencequery.VerifiedGitContribution

type contributionMemoryPin = memoryquery.ContributionPin

func (s *Service) prepareContextContributionMemory(ctx context.Context, repo domain.ContentHash, inclusion domain.BranchContext, snapshots []domain.Snapshot, verified []contextContributionProjection) (prContributionMemoryPlan, error) {
	records := make([]verifiedMemoryContribution, 0, len(verified))
	for _, contribution := range verified {
		records = append(records, verifiedMemoryContribution{Record: contribution.record, Valid: contribution.valid, Publications: contribution.publications})
	}
	pins := []contributionMemoryPin{}
	if len(records) > 0 {
		if ledger, ok := s.memoryPinReader().(outbound.PRContributionMemoryReader); ok {
			stored, err := ledger.ListPRContributionMemories(ctx, repo)
			if err != nil {
				return prContributionMemoryPlan{}, err
			}
			for _, pin := range stored {
				if pin.Validate() != nil || pin.RepoID != repo {
					return prContributionMemoryPlan{}, domain.ErrIntegrity
				}
				pins = append(pins, contributionMemoryPin(pin))
			}
		}
		if ledger, ok := s.memoryPinReader().(outbound.GitContributionMemoryReader); ok {
			stored, err := ledger.ListGitContributionMemories(ctx, repo)
			if err != nil {
				return prContributionMemoryPlan{}, err
			}
			for _, pin := range stored {
				if pin.Validate() != nil || pin.RepoID != repo {
					return prContributionMemoryPlan{}, domain.ErrIntegrity
				}
				pins = append(pins, contributionMemoryPin(pin))
			}
		}
	}
	return s.prepareVerifiedContributionMemory(ctx, repo, inclusion, snapshots, records, pins)
}

func (s *Service) contributionPublicationMemorySource(ctx context.Context, repo domain.ContentHash, capture domain.ContentHash, publication domain.HistoryEvent) (domain.ContentHash, error) {
	return contributionPublicationMemorySourceFromReader(ctx, s.memoryProjectionReader(), repo, capture, publication)
}

func contributionPublicationMemorySourceFromReader(ctx context.Context, reader outbound.MemoryObjectReader, repo domain.ContentHash, capture domain.ContentHash, publication domain.HistoryEvent) (domain.ContentHash, error) {
	if publication.Kind != "publish" || publication.RepoID != string(repo) || publication.Target != capture || publication.Source != capture || !publication.MemoryPinned {
		return "", domain.ErrIntegrity
	}
	source := publication.MemorySource
	if publication.MemoryHash == "" {
		return source, nil
	}
	if source == "" {
		source = capture
	}
	digest, err := reader.GetMemory(ctx, repo, publication.MemoryHash)
	if err != nil {
		return "", err
	}
	hash, err := domain.MemoryDigestHash(digest)
	if err != nil || hash != publication.MemoryHash || digest.SnapshotID != source {
		return "", domain.ErrIntegrity
	}
	return source, nil
}

func normalizePRContributionMemory(ctx context.Context, repo domain.ContentHash, verified []verifiedPRContribution, pins []domain.PRContributionMemory) ([]verifiedMemoryContribution, []contributionMemoryPin, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if len(verified) == 0 {
		return nil, nil, nil
	}
	if len(verified) > maxProjectionSnapshots || len(pins) > maxProjectionSnapshots {
		return nil, nil, domain.ErrMemoryProjectionLimit
	}
	records := make([]verifiedMemoryContribution, 0, len(verified))
	for _, contribution := range verified {
		if contribution.Record.Validate() != nil || contribution.Record.RepoID != repo {
			return nil, nil, domain.ErrIntegrity
		}
		record := contribution.Record
		records = append(records, verifiedMemoryContribution{
			Record: contributionMemoryRecord{ID: record.ID, RepoID: record.RepoID, DestinationBranchID: record.DestinationBranchID,
				SourceBranchID: record.SourceBranchID, Capture: record.Capture, OriginalHead: record.OriginalHead, IntegrationSHA: record.PR.MergeSHA},
			Valid: contribution.Valid, Publications: contribution.Publications,
		})
	}
	memories := make([]contributionMemoryPin, 0, len(pins))
	for _, pin := range pins {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if pin.Validate() != nil || pin.RepoID != repo {
			return nil, nil, domain.ErrIntegrity
		}
		memories = append(memories, contributionMemoryPin(pin))
	}
	return records, memories, nil
}

func (s *Service) prepareVerifiedPRContributionMemory(ctx context.Context, repo domain.ContentHash, inclusion domain.BranchContext, snapshots []domain.Snapshot, verified []verifiedPRContribution, pins []domain.PRContributionMemory) (prContributionMemoryPlan, error) {
	records, memories, err := normalizePRContributionMemory(ctx, repo, verified, pins)
	if err != nil {
		return prContributionMemoryPlan{}, err
	}
	return s.prepareVerifiedContributionMemory(ctx, repo, inclusion, snapshots, records, memories)
}

func normalizeGitContributionMemory(ctx context.Context, repo domain.ContentHash, verified []verifiedGitContribution, pins []domain.GitContributionMemory) ([]verifiedMemoryContribution, []contributionMemoryPin, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if len(verified) == 0 {
		return nil, nil, nil
	}
	if len(verified) > maxProjectionSnapshots || len(pins) > maxProjectionSnapshots {
		return nil, nil, domain.ErrMemoryProjectionLimit
	}
	records := make([]verifiedMemoryContribution, 0, len(verified))
	for _, contribution := range verified {
		if contribution.Record.Validate() != nil || contribution.Record.RepoID != repo {
			return nil, nil, domain.ErrIntegrity
		}
		record := contribution.Record
		records = append(records, verifiedMemoryContribution{
			Record: contributionMemoryRecord{ID: record.ID, RepoID: record.RepoID, DestinationBranchID: record.DestinationBranchID,
				SourceBranchID: record.SourceBranchID, Capture: record.Capture, OriginalHead: record.OriginalHead, IntegrationSHA: record.IntegrationSHA},
			Valid: contribution.Valid, Publications: contribution.Publications,
		})
	}
	memories := make([]contributionMemoryPin, 0, len(pins))
	for _, pin := range pins {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if pin.Validate() != nil || pin.RepoID != repo {
			return nil, nil, domain.ErrIntegrity
		}
		memories = append(memories, contributionMemoryPin(pin))
	}
	return records, memories, nil
}
