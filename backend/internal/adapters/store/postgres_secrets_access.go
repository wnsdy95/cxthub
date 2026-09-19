//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.SecretsAccessLocker = (*PostgresStore)(nil)

func (s *PostgresStore) LockSecretsAccess(ctx context.Context, workspace, actor string) error {
	tx, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || tx.owner != s || tx.readOnly || tx.repo == "" {
		return domain.ErrConflict
	}
	var id string
	if err := s.db(ctx).QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR SHARE`, workspace).Scan(&id); err != nil {
		return mapNoRows(err)
	}
	err := s.db(ctx).QueryRow(ctx, `SELECT user_id FROM memberships WHERE workspace_id=$1 AND user_id=$2 FOR SHARE`, workspace, actor).Scan(&id)
	// No membership grants no rights; the application still checks creator and role.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
