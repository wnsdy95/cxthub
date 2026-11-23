//go:build postgres

package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type repositoryRefPGPoisonBackend struct {
	*repositoryAssetPGPoisonFacade
}

func (*repositoryRefPGPoisonBackend) RecordHistory(context.Context, domain.HistoryEvent) error {
	panic("actual PostgreSQL history write reached the broad facade")
}

func (*repositoryRefPGPoisonBackend) RetryPRPromotion(context.Context, domain.ContentHash, string) error {
	panic("actual PostgreSQL PR retry reached the broad facade")
}

func (*repositoryRefPGPoisonBackend) EnableContextProtocol(context.Context, domain.ContentHash) error {
	panic("actual PostgreSQL protocol reached the broad facade")
}

func (*repositoryRefPGPoisonBackend) ListHistory(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error) {
	panic("actual PostgreSQL history read reached the broad facade")
}
