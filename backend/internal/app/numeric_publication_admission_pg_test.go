//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type numericPublicationPGStore struct {
	*store.PostgresStore
	writes          map[string]int
	unscopedWrites  int
	rejectSnapshot  domain.ContentHash
	snapshotFailure error
}

func (storage *numericPublicationPGStore) recordWrite(ctx context.Context, operation string) error {
	storage.writes[operation]++
	if !storage.InTransaction(ctx) || storage.InReadOnlyTransaction(ctx) {
		storage.unscopedWrites++
		return domain.ErrConflict
	}
	return nil
}

func (storage *numericPublicationPGStore) PutDoc(ctx context.Context, repo domain.ContentHash, document domain.SessionDoc) (bool, error) {
	if err := storage.recordWrite(ctx, "doc"); err != nil {
		return false, err
	}
	return storage.PostgresStore.PutDoc(ctx, repo, document)
}

func (storage *numericPublicationPGStore) PutVerifiedDoc(ctx context.Context, repo domain.ContentHash, document domain.VerifiedSessionDoc) (bool, error) {
	if err := storage.recordWrite(ctx, "verified_doc"); err != nil {
		return false, err
	}
	return storage.PostgresStore.PutVerifiedDoc(ctx, repo, document)
}

func (storage *numericPublicationPGStore) AdvanceRepositoryRevision(ctx context.Context, repo domain.ContentHash, pending bool) error {
	if err := storage.recordWrite(ctx, "revision"); err != nil {
		return err
	}
	return storage.PostgresStore.AdvanceRepositoryRevision(ctx, repo, pending)
}

func (storage *numericPublicationPGStore) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if err := storage.recordWrite(ctx, "audit"); err != nil {
		return err
	}
	return storage.PostgresStore.AppendOrganizationAudit(ctx, event)
}
