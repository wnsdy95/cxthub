package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (service *Service) readGitAdmissionSnapshot(ctx context.Context, repo domain.ContentHash) (*gitAdmissionSnapshot, error) {
	return service.gitIntegration().readGitAdmissionSnapshot(ctx, repo)
}

func (service *Service) readGitAdmissionSnapshotInRead(ctx context.Context, repo domain.ContentHash) (*gitAdmissionSnapshot, error) {
	return service.gitIntegration().readGitAdmissionSnapshotInRead(ctx, repo)
}

func (service *Service) captureGitAdmissionGeneration(ctx context.Context, repo domain.ContentHash, snapshot *gitAdmissionSnapshot) error {
	return service.gitIntegration().captureGitAdmissionGeneration(ctx, repo, snapshot)
}

func (service *Service) readGitAdmissionHistory(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) (*gitAdmissionSnapshot, error) {
	return service.gitIntegration().readGitAdmissionHistory(ctx, repo, history)
}

func (service *Service) readGitAdmissionSnapshotWithHistory(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) (*gitAdmissionSnapshot, error) {
	return service.gitIntegration().readGitAdmissionSnapshotWithHistory(ctx, repo, history)
}

func (service *Service) reuseGitAdmissionSnapshot(ctx context.Context, repo domain.ContentHash, frozen *gitAdmissionSnapshot) (*gitAdmissionSnapshot, error) {
	return service.gitIntegration().reuseGitAdmissionSnapshot(ctx, repo, frozen)
}

func (service *Service) readGitAdmissionEvidence(ctx context.Context, repo domain.ContentHash, lookups []gitAdmissionLookup) (*gitAdmissionSnapshot, error) {
	return service.gitIntegration().readGitAdmissionEvidence(ctx, repo, lookups)
}

func (service *Service) hydrateGitAdmissionEvidence(ctx context.Context, frozen *gitAdmissionSnapshot, publications []domain.HistoryEvent, receipt domain.GitHeadReceipt) (*gitAdmissionSnapshot, error) {
	return service.gitIntegration().hydrateGitAdmissionEvidence(ctx, frozen, publications, receipt)
}

func (service *Service) freezeGitAdmission(ctx context.Context, repo domain.ContentHash, publicationID, sourceHead, destinationName, destinationID string) (frozenGitAdmission, error) {
	return service.gitIntegration().freezeGitAdmission(ctx, repo, publicationID, sourceHead, destinationName, destinationID)
}

func (service *Service) persistGitAdmission(ctx context.Context, frozen frozenGitAdmission, prepared domain.GitContributionAdmission, authorization gitAdmissionAuthorization) (domain.GitContributionAdmission, error) {
	return service.gitIntegration().persistGitAdmission(ctx, frozen, prepared, authorization)
}

func (service *Service) persistGitAdmissionInWrite(write context.Context, frozen frozenGitAdmission, prepared domain.GitContributionAdmission, authorization gitAdmissionAuthorization, snapshot *gitAdmissionSnapshot) (domain.GitContributionAdmission, error) {
	return service.gitIntegration().persistGitAdmissionInWrite(write, frozen, prepared, authorization, snapshot)
}
