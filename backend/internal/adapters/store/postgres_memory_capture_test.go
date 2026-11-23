//go:build postgres

package store

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryCaptureTestKey struct {
	repo domain.ContentHash
	kind string
	hash domain.ContentHash
}

type memoryCaptureTestTransaction struct {
	pgx.Tx
	test      *testing.T
	objects   map[memoryCaptureTestKey][]byte
	queries   int
	copied    int
	beforeRow func()
	err       error
}

func (transaction *memoryCaptureTestTransaction) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	transaction.test.Helper()
	transaction.queries++
	if !strings.Contains(query, "octet_length(body.bytes)") || !strings.Contains(query, "CASE WHEN octet_length(body.bytes)<=$4 THEN body.bytes END") ||
		!strings.Contains(query, "owned.repo_id=$1 AND owned.kind=$2 AND owned.hash=$3") {
		transaction.test.Fatal("missing SQL byte/ownership fence", query)
	}
	if transaction.beforeRow != nil {
		transaction.beforeRow()
	}
	if err := ctx.Err(); err != nil {
		return memoryCaptureTestRow{err: err}
	}
	if transaction.err != nil {
		return memoryCaptureTestRow{err: transaction.err}
	}
	key := memoryCaptureTestKey{domain.ContentHash(arguments[0].(string)), arguments[1].(string), domain.ContentHash(arguments[2].(string))}
	body, exists := transaction.objects[key]
	if !exists {
		return memoryCaptureTestRow{err: pgx.ErrNoRows}
	}
	row := memoryCaptureTestRow{size: len(body)}
	if len(body) <= arguments[3].(int) {
		row.body = bytes.Clone(body)
		transaction.copied += len(body)
	}
	return row
}

type memoryCaptureTestRow struct {
	size int
	body []byte
	err  error
}

func (row memoryCaptureTestRow) Scan(destinations ...any) error {
	if row.err != nil {
		return row.err
	}
	*destinations[0].(*int) = row.size
	*destinations[1].(*[]byte) = row.body
	if len(destinations) == 4 {
		incarnation := "11111111-2222-3333-4444-555555555555"
		*destinations[2].(**string) = &incarnation
		*destinations[3].(*int64) = 0
	}
	return nil
}
