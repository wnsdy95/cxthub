package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *Service) ListHistory(ctx context.Context, repoID domain.ContentHash) ([]domain.HistoryEvent, error) {
	return s.branchOperations().ListHistory(ctx, repoID)
}

func (s *Service) RecordHistory(ctx context.Context, event domain.HistoryEvent) error {
	return s.historyRecorder().RecordHistory(ctx, event)
}

func (service *Service) recordHistoryWithDocumentPreparation(ctx context.Context, event domain.HistoryEvent) error {
	return service.historyRecorder().recordHistoryWithDocumentPreparation(ctx, event)
}

// A verification set lives for one operation, not on the service or store.
// Keys include repository ownership and both immutable snapshot/document IDs.
type historyDocumentKey struct {
	repo, snapshot domain.ContentHash
	document       domain.DocumentRef
}
type historyVerification map[historyDocumentKey]struct{}

func (s *Service) recordHistory(ctx context.Context, event domain.HistoryEvent, serverReceipt bool, verified historyVerification) error {
	return s.historyRecorder().recordHistory(ctx, event, serverReceipt, verified)
}

func (s *Service) recordHistoryPrepared(ctx context.Context, event domain.HistoryEvent, serverReceipt bool, verified historyVerification, prepare func(context.Context, []domain.ContentHash) (historyVerification, error)) error {
	return s.historyRecorder().recordHistoryPrepared(ctx, event, serverReceipt, verified, prepare)
}

func (s *Service) wakePublishedPRJobs(ctx context.Context, event domain.HistoryEvent) error {
	return s.historyRecorder().wakePublishedPRJobs(ctx, event)
}
