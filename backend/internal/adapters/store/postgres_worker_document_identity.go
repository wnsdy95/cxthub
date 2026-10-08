//go:build postgres

package store

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func lockNotificationPolicy(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cxt-identity-access',0))`)
	return err
}
func checkNotificationPolicy(ctx context.Context, tx interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, repository string) error {
	rows, err := tx.Query(ctx, `SELECT required_doc_identity FROM repos WHERE repository_id=$1 ORDER BY id FOR SHARE`, repository)
	if err != nil {
		return err
	}
	defer rows.Close()
	if err = outbound.CheckDocumentIdentityCompatibility(ctx, domain.DocumentIdentityLegacy); err != nil {
		return err
	}
	for rows.Next() {
		var required domain.DocumentIdentity
		if err = rows.Scan(&required); err != nil {
			return err
		}
		if err = outbound.CheckDocumentIdentityCompatibility(ctx, required); err != nil {
			return err
		}
	}
	return rows.Err()
}
