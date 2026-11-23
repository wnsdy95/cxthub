//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (storage *PostgresStore) requireDocDisplayRead(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !storage.InReadOnlyTransaction(ctx) {
		return domain.ErrConflict
	}
	var pinned bool
	if err := storage.db(ctx).QueryRow(ctx, `SELECT current_setting('transaction_isolation')='repeatable read'
 AND current_setting('transaction_read_only')='on'`).Scan(&pinned); err != nil {
		return err
	}
	if !pinned {
		return domain.ErrConflict
	}
	return nil
}
