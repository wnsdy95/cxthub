//go:build postgres

package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type settingsCodexFailurePG struct {
	*store.PostgresStore
	bodyError   error
	auditError  error
	bodyWrites  int
	auditWrites int
}

func (storage *settingsCodexFailurePG) PutSettingsBundle(ctx context.Context, repo domain.ContentHash, bundle domain.SettingsBundle) error {
	if !storage.InTransaction(ctx) {
		return errors.New("settings body escaped its PostgreSQL transaction")
	}
	if err := storage.PostgresStore.PutSettingsBundle(ctx, repo, bundle); err != nil {
		return err
	}
	storage.bodyWrites++
	return storage.bodyError
}

func (storage *settingsCodexFailurePG) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if !storage.InTransaction(ctx) {
		return errors.New("settings audit escaped its PostgreSQL transaction")
	}
	if err := storage.PostgresStore.AppendOrganizationAudit(ctx, event); err != nil {
		return err
	}
	storage.auditWrites++
	return storage.auditError
}
