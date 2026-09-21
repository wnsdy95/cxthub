//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.RepositoryAccessLocker = (*PostgresStore)(nil)

func (s *PostgresStore) LockRepositoryAccess(ctx context.Context, repository, actor string) error {
	tx, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || tx.owner != s || tx.readOnly || tx.repo == "" {
		return domain.ErrConflict
	}
	if _, err := s.db(ctx).Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended('cxt-identity-access',0))`); err != nil {
		return err
	}
	var id string
	if err := s.db(ctx).QueryRow(ctx, `SELECT id FROM repositories WHERE id=$1 FOR SHARE`, repository).Scan(&id); err != nil {
		return mapNoRows(err)
	}
	err := s.db(ctx).QueryRow(ctx, `SELECT user_id FROM memberships WHERE repository_id=$1 AND user_id=$2 FOR SHARE`, repository, actor).Scan(&id)
	// No membership grants no rights; the application still checks creator and role.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
