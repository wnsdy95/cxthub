package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (service *Service) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	scope := service.memoryResourceScope()
	scope.forceGeneral = true
	return scope.WithMemoryReadResources(ctx, repo, read)
}

func memoryReadForRepo[Result any](ctx context.Context, service *Service, repo domain.ContentHash, read func(context.Context) (Result, error)) (Result, error) {
	var result Result
	err := service.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		var err error
		result, err = repositoryReadForRepo(bound, service, repo, read)
		return err
	})
	if err != nil {
		var zero Result
		return zero, err
	}
	return result, nil
}
