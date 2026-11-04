//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type repositoryViewSourceBudgetQuery struct {
	args        []any
	columns     int
	jsonColumns int
	rows        int
	images      int
	imageBytes  int
	valueBytes  int
	firstKey    string
}

type repositoryViewSourceBudgetTx struct {
	pgx.Tx
	queries []*repositoryViewSourceBudgetQuery
}

func (observed *repositoryViewSourceBudgetTx) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	rows, err := observed.Tx.Query(ctx, query, args...)
	if err != nil || (!strings.Contains(query, "repository_view_source_images") && !strings.Contains(query, "repository_graph_source_images")) {
		return rows, err
	}
	record := &repositoryViewSourceBudgetQuery{args: append([]any(nil), args...), columns: len(rows.FieldDescriptions())}
	for _, field := range rows.FieldDescriptions() {
		if field.DataTypeOID == pgtype.JSONBOID || field.DataTypeOID == pgtype.JSONOID {
			record.jsonColumns++
		}
	}
	observed.queries = append(observed.queries, record)
	return &repositoryViewSourceBudgetRows{Rows: rows, record: record}, nil
}

type repositoryViewSourceBudgetRows struct {
	pgx.Rows
	record *repositoryViewSourceBudgetQuery
	closed bool
}

func (observed *repositoryViewSourceBudgetRows) Next() bool {
	if !observed.Rows.Next() {
		return false
	}
	observed.record.rows++
	fields := observed.Rows.FieldDescriptions()
	for index, raw := range observed.Rows.RawValues() {
		observed.record.valueBytes += len(raw)
		if raw != nil && (fields[index].DataTypeOID == pgtype.JSONBOID || fields[index].DataTypeOID == pgtype.JSONOID) {
			observed.record.images++
			observed.record.imageBytes += len(raw)
			if fields[index].DataTypeOID == pgtype.JSONBOID && fields[index].Format == pgx.BinaryFormatCode {
				observed.record.imageBytes--
			}
		}
		if observed.record.rows == 1 && observed.record.jsonColumns == 0 && index == 2 {
			observed.record.firstKey = string(raw)
		}
	}
	return true
}

func (observed *repositoryViewSourceBudgetRows) Close() {
	if observed.closed {
		return
	}
	observed.closed = true
	for observed.Next() {
	}
	observed.Rows.Close()
}

func repositoryViewSourceObservedReadPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, []*repositoryViewSourceBudgetQuery, error) {
	t.Helper()
	tx, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(tx)
	observed := &repositoryViewSourceBudgetTx{Tx: tx}
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: observed, owner: storage, readOnly: true})
	page, err := storage.RepositoryViewSourceChanges(bound, repo, request)
	return page, observed.queries, err
}

func TestPGRepositoryViewSourceBudgetDrainsOnlyDescriptorsAndFetchesExactPrefix(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	before := repositoryViewSourceCheckpointPG(t, ctx, storage, repo)
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO pending_contexts(repo_id,session_id,data)
		SELECT $1,'budget-'||lpad(position::text,2,'0'),jsonb_build_object('branch',repeat('x',700000))
		FROM generate_series(1,12) position`, string(repo))
	for _, mode := range []string{"baseline", "delta"} {
		t.Run(mode, func(t *testing.T) {
			request := domain.RepositoryViewSourceRequest{Version: 1, Limit: 1000}
			if mode == "delta" {
				request.After = &before
			}
			page, queries, err := repositoryViewSourceObservedReadPG(t, ctx, storage, repo, request)
			if err != nil {
				t.Fatal(err)
			}
			if page.Mode != mode || len(page.Entries) != 5 || page.NextCursor == "" || page.Checkpoint != nil {
				t.Fatalf("fixture did not split at the image-byte budget: mode=%s entries=%d cursor=%q checkpoint=%+v", page.Mode, len(page.Entries), page.NextCursor, page.Checkpoint)
			}
			transferredBytes := 0
			for _, query := range queries {
				transferredBytes += query.valueBytes
			}
			if transferredBytes > domain.MaxRepositoryViewSourcePageBytes {
				t.Fatalf("reading and draining a bounded page transferred %d result-value bytes", transferredBytes)
			}
			if len(queries) != 2 {
				t.Fatalf("want descriptor and exact-prefix payload phases, got %d source queries", len(queries))
			}
			descriptors, payloads := queries[0], queries[1]
			if descriptors.columns != 6 || descriptors.jsonColumns != 0 || descriptors.images != 0 || descriptors.imageBytes != 0 || descriptors.rows < 12 {
				t.Fatalf("descriptor drain transferred images or failed to exercise discarded rows: %+v", descriptors)
			}
			if payloads.columns != 1 || payloads.jsonColumns != 1 || payloads.rows != len(page.Entries) || payloads.images != len(page.Entries) {
				t.Fatalf("payload query transferred unselected rows: %+v", payloads)
			}
			sequences := make([]int64, len(page.Entries))
			kinds, keys := make([]string, len(page.Entries)), make([]string, len(page.Entries))
			emittedBytes := 0
			for index, entry := range page.Entries {
				sequences[index], kinds[index], keys[index] = entry.Sequence, entry.Kind, entry.Key
				emittedBytes += len(entry.Value)
			}
			wantArgs := []any{string(repo), page.Epoch, sequences, kinds, keys}
			if !reflect.DeepEqual(payloads.args, wantArgs) {
				t.Fatalf("payload selector differs from emitted exact identities: got=%+v want=%+v", payloads.args, wantArgs)
			}
			if payloads.imageBytes != emittedBytes || descriptors.valueBytes+payloads.valueBytes > domain.MaxRepositoryViewSourcePageBytes {
				t.Fatalf("discarded payloads crossed the connection: descriptors=%d payload=%d image=%d emitted=%d", descriptors.valueBytes, payloads.valueBytes, payloads.imageBytes, emittedBytes)
			}
		})
	}
}

func TestPGRepositoryViewSourceBudgetRejectsCompressibleOversizeKeys(t *testing.T) {
	for _, tombstone := range []bool{false, true} {
		name := "live"
		if tombstone {
			name = "tombstone"
		}
		t.Run(name, func(t *testing.T) {
			storage, ctx, repo := catalogPG(t)
			before := repositoryViewSourceCheckpointPG(t, ctx, storage, repo)
			key := strings.Repeat("a", 13000)
			tx := catalogTxPG(t, ctx, storage)
			catalogExecPG(t, ctx, tx, `INSERT INTO pending_contexts(repo_id,session_id,data) VALUES($1,$2,'{}')`, string(repo), key)
			if tombstone {
				catalogExecPG(t, ctx, tx, `DELETE FROM pending_contexts WHERE repo_id=$1 AND session_id=$2`, string(repo), key)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			request := domain.RepositoryViewSourceRequest{Version: 1, Limit: 1}
			if tombstone {
				request.After = &before
			}
			page, queries, err := repositoryViewSourceObservedReadPG(t, ctx, storage, repo, request)
			if !errors.Is(err, domain.ErrRepositoryViewSourceLimit) || !reflect.DeepEqual(page, domain.RepositoryViewSourcePage{}) {
				t.Fatalf("oversize key did not fail closed: err=%v page=%+v", err, page)
			}
			if len(queries) != 1 || queries[0].jsonColumns != 0 || queries[0].firstKey != "" || queries[0].valueBytes >= len(key) {
				t.Fatalf("oversize key/image crossed the descriptor boundary: %+v", queries)
			}
		})
	}
}

func TestPGRepositoryViewSourceBudgetChecksEscapedCursorLength(t *testing.T) {
	for _, test := range []struct {
		name   string
		length int
		want   error
	}{
		{"replayable", 1024, nil},
		{"escaped_overflow", 3000, domain.ErrRepositoryViewSourceLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage, ctx, repo := catalogPG(t)
			key := strings.Repeat("\x01", test.length)
			if len(key) > domain.MaxRepositoryViewSourceKeyBytes {
				t.Fatal("fixture must reach cursor encoding without exceeding the key-byte limit")
			}
			catalogExecPG(t, ctx, storage.pool, `INSERT INTO pending_contexts(repo_id,session_id,data) VALUES($1,$2,'{}')`, string(repo), key)
			page, queries, err := repositoryViewSourceObservedReadPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
			if !errors.Is(err, test.want) {
				t.Fatalf("escaped cursor error=%v want=%v", err, test.want)
			}
			if test.want != nil {
				if !reflect.DeepEqual(page, domain.RepositoryViewSourcePage{}) || len(queries) != 1 || queries[0].images != 0 {
					t.Fatalf("unreplayable cursor returned a page or fetched payloads: page=%+v queries=%+v", page, queries)
				}
				return
			}
			if len(page.Entries) != 1 || page.Entries[0].Key != key || page.NextCursor == "" || len(page.NextCursor) > 16<<10 {
				t.Fatalf("bounded escaped key did not produce a replayable page: entries=%d cursor_bytes=%d", len(page.Entries), len(page.NextCursor))
			}
			cursor, err := decodeRepositoryViewSourceCursor(page.NextCursor)
			if err != nil || cursor.Key != key {
				t.Fatalf("generated escaped cursor rejected or changed key: %v", err)
			}
			request := domain.RepositoryViewSourceRequest{Version: 1, Cursor: page.NextCursor, Limit: 1}
			if err := request.Validate(); err != nil {
				t.Fatal(err)
			}
			entries, _ := repositoryViewSourceDrainPG(t, ctx, storage, repo, page, 1)
			seen := 0
			for _, entry := range entries {
				if entry.Kind == "pending" && entry.Key == key {
					seen++
				}
			}
			if seen != 1 {
				t.Fatalf("escaped-key continuation duplicated or omitted pending: count=%d", seen)
			}
		})
	}
}
