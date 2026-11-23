//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type wave42PGRepositoryPGStore struct {
	*store.PostgresStore
	rejectRevision error
	rejectAudit    error
	commitACK      error
	readACK        error
	writes         int
}

func (storage *wave42PGRepositoryPGStore) WithinRepository(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	storage.writes++
	err := storage.PostgresStore.WithinRepository(ctx, repo, write)
	if err == nil && storage.commitACK != nil {
		err, storage.commitACK = storage.commitACK, nil
	}
	return err
}

func (storage *wave42PGRepositoryPGStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	err := storage.PostgresStore.WithinReadSnapshot(ctx, read)
	if err == nil && storage.readACK != nil {
		return storage.readACK
	}
	return err
}

func (storage *wave42PGRepositoryPGStore) AdvanceRepositoryRevision(ctx context.Context, repo domain.ContentHash, pending bool) error {
	if err := storage.PostgresStore.AdvanceRepositoryRevision(ctx, repo, pending); err != nil {
		return err
	}
	return storage.rejectRevision
}

func (storage *wave42PGRepositoryPGStore) AdvanceEvidenceRevision(ctx context.Context, repo domain.ContentHash) error {
	if err := storage.PostgresStore.AdvanceEvidenceRevision(ctx, repo); err != nil {
		return err
	}
	return storage.rejectRevision
}

func (storage *wave42PGRepositoryPGStore) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if err := storage.PostgresStore.AppendOrganizationAudit(ctx, event); err != nil {
		return err
	}
	return storage.rejectAudit
}
