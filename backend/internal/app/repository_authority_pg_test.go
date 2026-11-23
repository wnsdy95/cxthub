//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var (
	_ RepositoryAuthorityMetadata      = (*store.PostgresStore)(nil)
	_ RepositoryAuthorityRepositories  = (*store.PostgresStore)(nil)
	_ RepositoryAuthorityTeams         = (*store.PostgresStore)(nil)
	_ RepositoryAuthorityOrganizations = (*store.PostgresStore)(nil)
	_ RepositoryAuthorityAudit         = (*store.PostgresStore)(nil)
	_ outbound.RepositoryAccessLocker  = (*store.PostgresStore)(nil)
)

type repositoryAuthorityIdentityHoldPG struct {
	*store.PostgresStore
	ready   chan struct{}
	release <-chan struct{}
}

func (storage *repositoryAuthorityIdentityHoldPG) WithinIdentity(ctx context.Context, operation func(context.Context) error) error {
	return storage.PostgresStore.WithinIdentity(ctx, func(bound context.Context) error {
		if err := operation(bound); err != nil {
			return err
		}
		close(storage.ready)
		select {
		case <-storage.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

type repositoryAuthorityReadsPG struct {
	RepositoryAuthorityRepositories
	reads int
}

func (reader *repositoryAuthorityReadsPG) GetRepository(ctx context.Context, repository string) (domain.Repository, error) {
	reader.reads++
	return reader.RepositoryAuthorityRepositories.GetRepository(ctx, repository)
}

func (reader *repositoryAuthorityReadsPG) ListMembers(ctx context.Context, repository string) ([]domain.Membership, error) {
	reader.reads++
	return reader.RepositoryAuthorityRepositories.ListMembers(ctx, repository)
}
