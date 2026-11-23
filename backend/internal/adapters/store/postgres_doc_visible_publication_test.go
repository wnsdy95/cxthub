//go:build postgres

package store

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docVisibleFailTx struct {
	pgx.Tx
	point   string
	err     error
	batches int
}

func (transaction *docVisibleFailTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if transaction.point == "descriptor" && strings.Contains(sql, "INSERT INTO doc_visible_generations_v1") {
		return pgconn.CommandTag{}, transaction.err
	}
	return transaction.Tx.Exec(ctx, sql, args...)
}

func (transaction *docVisibleFailTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	for _, query := range batch.QueuedQueries {
		if len(query.Arguments) == 3 {
			if raw, ok := query.Arguments[2].([]byte); ok && bytes.Contains(raw, []byte(`"units":[`)) {
				transaction.batches++
				if transaction.point == "objects" && transaction.batches == 2 {
					return docVisibleBatchResult{err: transaction.err}
				}
				break
			}
		}
	}
	return transaction.Tx.SendBatch(ctx, batch)
}

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
