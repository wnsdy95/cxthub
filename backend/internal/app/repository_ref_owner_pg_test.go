//go:build postgres

package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type repositoryRefProbePG struct {
	*store.PostgresStore
	calls          map[string]int
	transactions   int
	failAt         string
	failure        error
	replaceSession string
	replacement    domain.ContentHash
}

func (storage *repositoryRefProbePG) WithinRepository(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	if !storage.InTransaction(ctx) {
		storage.transactions++
	}
	return storage.PostgresStore.WithinRepository(ctx, repo, write)
}

func (storage *repositoryRefProbePG) note(ctx context.Context, phase string) error {
	storage.calls[phase]++
	if !storage.InTransaction(ctx) || storage.InReadOnlyTransaction(ctx) || phase != "revision" && phase != "audit" && ctx.Value(afterCommitKey{}) == nil {
		return errors.New("actual ref command escaped its original writable scope")
	}
	return nil
}

func (storage *repositoryRefProbePG) after(phase string) error {
	if storage.failAt == phase {
		return storage.failure
	}
	return nil
}

func (storage *repositoryRefProbePG) EnqueueNotification(ctx context.Context, delivery outbound.NotificationDelivery) error {
	if err := storage.note(ctx, "outbox"); err != nil {
		return err
	}
	if err := storage.PostgresStore.EnqueueNotification(ctx, delivery); err != nil {
		return err
	}
	return storage.after("outbox")
}

func (storage *repositoryRefProbePG) AdvanceRepositoryRevision(ctx context.Context, repo domain.ContentHash, pending bool) error {
	if err := storage.note(ctx, "revision"); err != nil {
		return err
	}
	if err := storage.PostgresStore.AdvanceRepositoryRevision(ctx, repo, pending); err != nil {
		return err
	}
	return storage.after("revision")
}

func (storage *repositoryRefProbePG) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if err := storage.note(ctx, "audit"); err != nil {
		return err
	}
	if err := storage.PostgresStore.AppendOrganizationAudit(ctx, event); err != nil {
		return err
	}
	return storage.after("audit")
}
