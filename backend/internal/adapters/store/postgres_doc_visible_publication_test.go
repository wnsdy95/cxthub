//go:build postgres

package store

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docVisiblePGImage(t *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) string {
	t.Helper()
	var image string
	if err := storage.db(ctx).QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT to_jsonb(visible) FROM doc_visible_generations_v1 visible WHERE doc_hash=$1),
 (SELECT to_jsonb(ranges) FROM doc_range_generations_v2 ranges WHERE doc_hash=$1),
 (SELECT jsonb_agg(to_jsonb(object) ORDER BY hash) FROM doc_range_objects_v2 object WHERE doc_hash=$1),
 (SELECT to_jsonb(header) FROM doc_range_indexes_v1 header WHERE hash=$1),
 (SELECT jsonb_agg(to_jsonb(page) ORDER BY ordinal) FROM doc_range_pages_v1 page WHERE doc_hash=$1)
 )::text`, hash).Scan(&image); err != nil {
		t.Fatal(err)
	}
	return image
}
