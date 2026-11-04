package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type revisionScopeKey struct{}

func (service *Service) repositoryWork() *repositoryWork {
	return newRepositoryWork(service.meta, service.repositories, service.DocumentIdentitiesSupported())
}

func evidenceWriteContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, revisionScopeKey{}, "evidence")
}
func pendingWriteContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, revisionScopeKey{}, "pending")
}
func revisionWrite(ctx context.Context, service *Service, repo domain.ContentHash) error {
	return service.repositoryWork().revisionWrite(ctx, repo)
}

type afterCommitKey struct{}
type afterCommitActions struct{ actions []func() }

func repositoryWrite[Result any](ctx context.Context, service *Service, repo domain.ContentHash, write func(context.Context) (Result, error)) (Result, error) {
	return repositoryWriteWithWork(ctx, service.repositoryWork(), repo, write)
}

func repositoryWriteError(ctx context.Context, service *Service, repo domain.ContentHash, write func(context.Context) error) error {
	return repositoryWriteErrorWithWork(ctx, service.repositoryWork(), repo, write)
}

func repositoryRead[Result any](ctx context.Context, service *Service, read func(context.Context) (Result, error)) (Result, error) {
	return repositoryReadWithWork(ctx, service.repositoryWork(), read)
}

func afterRepositoryCommit(ctx context.Context, fn func()) {
	if after, ok := ctx.Value(afterCommitKey{}).(*afterCommitActions); ok {
		after.actions = append(after.actions, fn)
		return
	}
	fn()
}
