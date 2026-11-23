//go:build postgres

package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type documentPublicationProbePG struct {
	*store.PostgresStore
	calls         map[string]int
	transactions  int
	failAt        string
	failure       error
	sourceFailure error
	cancel        context.CancelFunc
}

func (storage *documentPublicationProbePG) WithinRepository(ctx context.Context, repo domain.ContentHash, operation func(context.Context) error) error {
	if !storage.InTransaction(ctx) {
		storage.transactions++
	}
	return storage.PostgresStore.WithinRepository(ctx, repo, operation)
}

func (storage *documentPublicationProbePG) note(ctx context.Context, phase string) error {
	storage.calls[phase]++
	if !storage.InTransaction(ctx) || storage.InReadOnlyTransaction(ctx) {
		return errors.New("publication port escaped the writable repository transaction")
	}
	return nil
}

func (storage *documentPublicationProbePG) after(phase string) error {
	if storage.failAt == phase {
		if storage.cancel != nil {
			storage.cancel()
			return nil
		}
		return storage.failure
	}
	return nil
}

func (storage *documentPublicationProbePG) PutDoc(ctx context.Context, repo domain.ContentHash, document domain.SessionDoc) (bool, error) {
	if err := storage.note(ctx, "legacy_doc"); err != nil {
		return false, err
	}
	return false, errors.New("complete publication fell back to the legacy document writer")
}

func (storage *documentPublicationProbePG) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := storage.note(ctx, "legacy_read"); err != nil {
		return domain.SessionDoc{}, err
	}
	return storage.PostgresStore.GetDoc(ctx, repo, hash)
}

func (storage *documentPublicationProbePG) VerifyStoredDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	if err := storage.note(ctx, "source"); err != nil {
		return domain.VerifiedDocReference{}, err
	}
	if storage.sourceFailure != nil {
		return domain.VerifiedDocReference{}, storage.sourceFailure
	}
	return storage.PostgresStore.VerifyStoredDoc(ctx, repo, hash)
}

func (storage *documentPublicationProbePG) PutVerifiedDoc(ctx context.Context, repo domain.ContentHash, document domain.VerifiedSessionDoc) (bool, error) {
	if err := storage.note(ctx, "verified_doc"); err != nil {
		return false, err
	}
	stored, err := storage.PostgresStore.PutVerifiedDoc(ctx, repo, document)
	if err != nil {
		return false, err
	}
	return stored, storage.after("verified_doc")
}

func (storage *documentPublicationProbePG) PutChunks(ctx context.Context, repo domain.ContentHash, chunks map[domain.ContentHash][]byte) (int, int, error) {
	if err := storage.note(ctx, "chunks"); err != nil {
		return 0, 0, err
	}
	stored, deduped, err := storage.PostgresStore.PutChunks(ctx, repo, chunks)
	if err != nil {
		return 0, 0, err
	}
	return stored, deduped, storage.after("chunks")
}

func (storage *documentPublicationProbePG) AdvanceRepositoryRevision(ctx context.Context, repo domain.ContentHash, pending bool) error {
	if err := storage.note(ctx, "revision"); err != nil {
		return err
	}
	if err := storage.PostgresStore.AdvanceRepositoryRevision(ctx, repo, pending); err != nil {
		return err
	}
	return storage.after("revision")
}

func (storage *documentPublicationProbePG) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if err := storage.note(ctx, "audit"); err != nil {
		return err
	}
	if err := storage.PostgresStore.AppendOrganizationAudit(ctx, event); err != nil {
		return err
	}
	return storage.after("audit")
}
