package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type writeActionKey struct{}

func writeAction(ctx context.Context, action string) context.Context {
	return context.WithValue(ctx, writeActionKey{}, action)
}

func (service *Service) authorizeRepositoryWrite(ctx context.Context, repo domain.ContentHash) error {
	return service.repositoryWork().authorizeRepositoryWrite(ctx, repo)
}
