//go:build postgres

package store

import "github.com/wnsdy95/cxthub/backend/internal/ports/outbound"

func (storage *PostgresStore) ConfigureTransientSpoolBudget(limit int64) (func() error, error) {
	return configureTransientSpoolBudget(limit)
}

func (storage *PostgresStore) TransientSpoolBudgetUsage() outbound.TransientSpoolBudgetUsage {
	return transientSpoolBudgetUsage()
}

var _ outbound.TransientSpoolBudgetStore = (*PostgresStore)(nil)
