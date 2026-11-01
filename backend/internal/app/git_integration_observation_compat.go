package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (scans *GitScans) freezeGitAdmissionBatch(ctx context.Context, claim domain.GitHeadScan, identity *domain.GitProviderObservation, fence GitReadFence) (*frozenGitAdmissionBatch, error) {
	return scans.integration().freezeGitAdmissionBatch(ctx, claim, identity, fence)
}

func (scans *GitScans) prepareGitAdmissionBatch(ctx context.Context, frozen *frozenGitAdmissionBatch, receipt *domain.GitHeadReceipt) (*preparedGitAdmissionBatch, error) {
	return scans.integration().prepareGitAdmissionBatch(ctx, frozen, receipt)
}

func (scans *GitScans) persistGitAdmissionBatch(ctx context.Context, batch *preparedGitAdmissionBatch, current *gitAdmissionSnapshot) error {
	return scans.integration().persistGitAdmissionBatch(ctx, batch, current)
}

func (scans *GitScans) freezeGitHeadContext(ctx context.Context, claim domain.GitHeadScan, identity *domain.GitProviderObservation, batch *frozenGitAdmissionBatch) (*gitAdmissionSnapshot, error) {
	return scans.integration().freezeGitHeadContext(ctx, claim, identity, batch)
}

func (scans *GitScans) persistGitContextHeads(ctx context.Context, receipt *domain.GitHeadReceipt, heads []domain.GitContextHead, current *gitAdmissionSnapshot) error {
	return scans.integration().persistGitContextHeads(ctx, receipt, heads, current)
}

func (scans *GitScans) readGitAdmissionSources(ctx context.Context, frozen *frozenGitAdmissionBatch) ([]domain.GitRefObservation, error) {
	return scans.integration().readGitAdmissionSources(ctx, frozen)
}

func (scans *GitScans) discoverGitAssociations(ctx context.Context, repo domain.Repo) (bool, error) {
	return scans.integration().discoverGitAssociations(ctx, repo)
}
