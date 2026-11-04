//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func readDocDisplayCatalogState(ctx context.Context, transaction pgx.Tx, source domain.ContentHash) (string, string, error) {
	var epoch, state string
	if err := transaction.QueryRow(ctx, `SELECT catalog_epoch::text,state FROM doc_display_catalog_lifecycle_v1
 WHERE source_hash=$1 FOR UPDATE NOWAIT`, source).Scan(&epoch, &state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", domain.ErrIntegrity
		}
		return "", "", err
	}
	if domain.ValidateDocDisplayCatalogEpoch(epoch) != nil || state != "live" && state != "retiring" && state != "retired" {
		return "", "", domain.ErrIntegrity
	}
	return epoch, state, ctx.Err()
}
