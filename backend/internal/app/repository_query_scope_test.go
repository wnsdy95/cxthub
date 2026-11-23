package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type repositoryScopeReadKey struct{}

type repositoryScopeReadStore struct {
	*store.FSStore
	snapshots, metadataReads, revisionReads int
	finishFailure                           error
}

func (storage *repositoryScopeReadStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if ctx.Value(repositoryScopeReadKey{}) == storage {
		return read(ctx)
	}
	storage.snapshots++
	return errors.Join(read(context.WithValue(ctx, repositoryScopeReadKey{}, storage)), storage.finishFailure)
}

func (storage *repositoryScopeReadStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	storage.metadataReads++
	if ctx.Value(repositoryScopeReadKey{}) != storage {
		return domain.Repo{}, domain.ErrIntegrity
	}
	return storage.FSStore.GetRepo(ctx, repo)
}

func (storage *repositoryScopeReadStore) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	storage.revisionReads++
	if ctx.Value(repositoryScopeReadKey{}) != storage {
		return domain.RepositoryRevision{}, domain.ErrIntegrity
	}
	return storage.FSStore.RepositoryRevision(ctx, repo)
}
