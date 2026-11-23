package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memorySelectionHTTPBackend struct {
	Backend
	repo  domain.Repo
	calls []domain.HistoryEvent
	err   error
}

func (b *memorySelectionHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return b.repo, nil
}
func (b *memorySelectionHTTPBackend) RecordHistory(_ context.Context, e domain.HistoryEvent) error {
	b.calls = append(b.calls, e)
	return b.err
}
