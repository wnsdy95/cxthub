//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type rootPublicationBatchTestTx struct {
	pgx.Tx
	query func(context.Context, string, ...any) (pgx.Rows, error)
}

func (transaction *rootPublicationBatchTestTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return transaction.query(ctx, sql, args...)
}

type rootPublicationBatchTestRows struct {
	pgx.Rows
	hash     domain.ContentHash
	size     int
	body     []byte
	count    int
	index    int
	closed   bool
	terminal error
}

func (rows *rootPublicationBatchTestRows) Next() bool { return rows.index < rows.count }
func (rows *rootPublicationBatchTestRows) Close()     { rows.closed = true }
func (rows *rootPublicationBatchTestRows) Err() error { return rows.terminal }
func (rows *rootPublicationBatchTestRows) Scan(destinations ...any) error {
	*destinations[0].(*domain.ContentHash) = rows.hash
	*destinations[1].(*int) = rows.size
	if len(destinations) == 3 {
		*destinations[2].(*[]byte) = bytes.Clone(rows.body)
	}
	rows.index++
	return nil
}

func TestRootPublicationBatchRejectsInvalidRows(test *testing.T) {
	for _, scenario := range []string{"missing-size", "wrong-hash", "oversize", "zero", "metadata-error", "missing-body", "changed-size", "short-body", "duplicate", "body-error"} {
		test.Run(scenario, func(test *testing.T) {
			storage := &PostgresStore{}
			repo := domain.HashContent([]byte("repository"))
			hash := domain.HashContent([]byte("body"))
			metadata := &rootPublicationBatchTestRows{hash: hash, size: 4, count: 1}
			payload := &rootPublicationBatchTestRows{hash: hash, size: 4, body: []byte("body"), count: 1}
			injected := errors.New("query interrupted")
			switch scenario {
			case "missing-size":
				metadata.count = 0
			case "wrong-hash":
				metadata.hash = domain.HashContent([]byte("foreign"))
			case "oversize":
				metadata.size = maxStoredRangeChunkBytes + 1
			case "zero":
				metadata.size = 0
			case "metadata-error":
				metadata.terminal = injected
			case "missing-body":
				payload.count = 0
			case "changed-size":
				payload.size = 5
			case "short-body":
				payload.body = nil
			case "duplicate":
				payload.count = 2
			case "body-error":
				payload.terminal = injected
			}
			queries := 0
			transaction := &rootPublicationBatchTestTx{query: func(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
				queries++
				if strings.Contains(sql, "FOR SHARE OF rb,b") {
					if strings.Contains(sql, "substring") || !strings.Contains(sql, "ORDER BY b.hash") {
						test.Fatal("metadata locking transferred payload or lost lock order")
					}
					return metadata, nil
				}
				if !metadata.closed || !strings.Contains(sql, "CASE WHEN octet_length(b.bytes)<=$3") {
					test.Fatal("payload read lost metadata lifetime or SQL size fence")
				}
				return payload, nil
			}}
			ctx := context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage, repo: repo})
			reader, err := storage.retainedRootChunkReader(ctx, repo, []domain.ContentHash{hash})
			if err == nil {
				_, err = reader(ctx, hash)
			}
			if err == nil || !metadata.closed || queries == 2 && !payload.closed {
				test.Fatal("invalid stream accepted or rows leaked", err)
			}
			if strings.HasSuffix(scenario, "error") && !errors.Is(err, injected) {
				test.Fatal("query error replaced", err)
			}
		})
	}
}

func TestRootPublicationBatchCachedReadKeepsContextBoundary(test *testing.T) {
	storage := &PostgresStore{}
	repo := domain.HashContent([]byte("repository"))
	hash := domain.HashContent([]byte("body"))
	queries := 0
	transaction := &rootPublicationBatchTestTx{query: func(context.Context, string, ...any) (pgx.Rows, error) {
		queries++
		return &rootPublicationBatchTestRows{hash: hash, size: 4, body: []byte("body"), count: 1}, nil
	}}
	ctx := context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage, repo: repo})
	reader, err := storage.retainedRootChunkReader(ctx, repo, []domain.ContentHash{hash, hash})
	if err != nil {
		test.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		body, err := reader(ctx, hash)
		if err != nil || string(body) != "body" {
			test.Fatal("bounded repeated chunk", err)
		}
	}
	if queries != 2 {
		test.Fatal("duplicate hash was fetched twice", queries)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := reader(canceled, hash); !errors.Is(err, context.Canceled) {
		test.Fatal("cache hid cancellation", err)
	}
	for _, foreign := range []context.Context{context.Background(), outbound.WithDocReadOnly(ctx), context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage, repo: repo})} {
		if _, err := reader(foreign, hash); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("cache escaped original write transaction", err)
		}
	}
	if _, err := reader(ctx, domain.HashContent([]byte("unrecorded"))); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("reader silently expanded closure", err)
	}
	if queries != 2 {
		test.Fatal("invalid read queried storage", queries)
	}
}
