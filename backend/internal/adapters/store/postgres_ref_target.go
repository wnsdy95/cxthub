//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (storage *PostgresStore) HasOrdinaryTagTarget(ctx context.Context, repo, target domain.ContentHash) (bool, error) {
	have, err := storage.HasSnapshots(ctx, repo, []domain.ContentHash{target})
	return len(have) == 1 && have[0] == target, err
}
