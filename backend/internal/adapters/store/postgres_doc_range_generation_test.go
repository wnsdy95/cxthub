//go:build postgres

package store

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func generationCountsPG(t *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) [2]int {
	t.Helper()
	var counts [2]int
	if err := storage.db(ctx).QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$1),
 (SELECT count(*) FROM doc_range_objects_v2 WHERE doc_hash=$1)`, hash).Scan(&counts[0], &counts[1]); err != nil {
		t.Fatal(err)
	}
	return counts
}
