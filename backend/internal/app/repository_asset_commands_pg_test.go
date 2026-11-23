//go:build postgres

package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type repositoryAssetFailurePG struct {
	*store.PostgresStore
	repo            domain.ContentHash
	failAt          string
	failure         error
	calls           []string
	revisionAtAudit domain.RepositoryRevision
	identityAtAudit domain.DocumentIdentity
}

func (storage *repositoryAssetFailurePG) wrote(ctx context.Context, phase string) error {
	if !storage.InTransaction(ctx) {
		return errors.New("asset SQL write escaped its repository transaction")
	}
	storage.calls = append(storage.calls, phase)
	if phase == storage.failAt {
		return storage.failure
	}
	return nil
}

func (storage *repositoryAssetFailurePG) UpdateRepoAbout(ctx context.Context, repo domain.ContentHash, description, website string, topics []string) error {
	if err := storage.PostgresStore.UpdateRepoAbout(ctx, repo, description, website, topics); err != nil {
		return err
	}
	return storage.wrote(ctx, "about")
}

func (storage *repositoryAssetFailurePG) UpdateRepoConfig(ctx context.Context, repo domain.ContentHash, branch *string, protected *bool) error {
	if err := storage.PostgresStore.UpdateRepoConfig(ctx, repo, branch, protected); err != nil {
		return err
	}
	return storage.wrote(ctx, "config")
}

func (storage *repositoryAssetFailurePG) PutSettingsBundle(ctx context.Context, repo domain.ContentHash, bundle domain.SettingsBundle) error {
	if err := storage.PostgresStore.PutSettingsBundle(ctx, repo, bundle); err != nil {
		return err
	}
	return storage.wrote(ctx, "settings")
}

func (storage *repositoryAssetFailurePG) CompareAndSwapSecrets(ctx context.Context, repo domain.ContentHash, expected, next []byte) error {
	if err := storage.PostgresStore.CompareAndSwapSecrets(ctx, repo, expected, next); err != nil {
		return err
	}
	return storage.wrote(ctx, "cas")
}

func (storage *repositoryAssetFailurePG) EnqueueNotification(ctx context.Context, delivery outbound.NotificationDelivery) error {
	if err := storage.PostgresStore.EnqueueNotification(ctx, delivery); err != nil {
		return err
	}
	return storage.wrote(ctx, "outbox")
}

func (storage *repositoryAssetFailurePG) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if err := storage.PostgresStore.AppendOrganizationAudit(ctx, event); err != nil {
		return err
	}
	var err error
	storage.revisionAtAudit, err = storage.PostgresStore.RepositoryRevision(ctx, storage.repo)
	if err != nil {
		return err
	}
	metadata, err := storage.PostgresStore.GetRepo(ctx, storage.repo)
	if err != nil {
		return err
	}
	storage.identityAtAudit = metadata.RequiredDocIdentity
	return storage.wrote(ctx, "audit")
}
