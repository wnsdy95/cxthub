//go:build postgres

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type ownedDocPrefetchBatchPG struct {
	bytes int
	rows  int
}

type ownedDocPrefetchTxPG struct {
	pgx.Tx
	batches []*ownedDocPrefetchBatchPG
}

func (tx *ownedDocPrefetchTxPG) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := tx.Tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	batch := &ownedDocPrefetchBatchPG{}
	tx.batches = append(tx.batches, batch)
	return &ownedDocPrefetchRowsPG{Rows: rows, batch: batch}, nil
}

func (tx *ownedDocPrefetchTxPG) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	batch := &ownedDocPrefetchBatchPG{}
	tx.batches = append(tx.batches, batch)
	return &ownedDocPrefetchRowPG{Row: tx.Tx.QueryRow(ctx, sql, args...), batch: batch}
}

type ownedDocPrefetchRowsPG struct {
	pgx.Rows
	batch *ownedDocPrefetchBatchPG
}

func (rows *ownedDocPrefetchRowsPG) Scan(dest ...any) error {
	if err := rows.Rows.Scan(dest...); err != nil {
		return err
	}
	rows.batch.bytes += len(*dest[len(dest)-1].(*[]byte))
	rows.batch.rows++
	return nil
}

type ownedDocPrefetchRowPG struct {
	pgx.Row
	batch *ownedDocPrefetchBatchPG
}

func (row *ownedDocPrefetchRowPG) Scan(dest ...any) error {
	if err := row.Row.Scan(dest...); err != nil {
		return err
	}
	row.batch.bytes += len(*dest[len(dest)-1].(*[]byte))
	row.batch.rows++
	return nil
}

func ownedDocPrefetchPG(t *testing.T) (*PostgresStore, context.Context, *ownedDocPrefetchTxPG, domain.ContentHash) {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE blobs(hash text PRIMARY KEY, bytes bytea NOT NULL) ON COMMIT DROP;
 CREATE TEMP TABLE repo_blobs(repo_id text,kind text,hash text,PRIMARY KEY(repo_id,kind,hash)) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	storage := &PostgresStore{}
	counted := &ownedDocPrefetchTxPG{Tx: tx}
	repo := domain.HashContent([]byte(t.Name()))
	ctx = context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: counted, owner: storage, repo: repo})
	return storage, ctx, counted, repo
}

func putOwnedDocPrefetchChunkPG(t *testing.T, ctx context.Context, tx pgx.Tx, repo domain.ContentHash, body, stored []byte) domain.ContentHash {
	t.Helper()
	hash := domain.HashContent(body)
	if _, err := tx.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(hash), stored); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, string(repo), string(hash)); err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestPGOwnedDocChunkPrefetchByteWindow(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("compressed=%v", compressed), func(t *testing.T) {
			storage, ctx, tx, repo := ownedDocPrefetchPG(t)
			var chunks []domain.ContentHash
			for ordinal := 0; ordinal < 40; ordinal++ {
				body := bytes.Repeat([]byte{byte(ordinal + 1)}, domain.ChunkTarget)
				stored := body
				if compressed {
					stored = docCompress(body)
				}
				chunks = append(chunks, putOwnedDocPrefetchChunkPG(t, ctx, tx, repo, body, stored))
			}
			read := storage.ownedDocChunkReader(repo, chunks)
			for _, hash := range chunks {
				raw, err := read(ctx, hash)
				if err != nil {
					t.Fatal(err)
				}
				body, err := docDecompress(raw)
				if err != nil || len(body) != domain.ChunkTarget || domain.HashContent(body) != hash {
					t.Fatalf("normal chunk changed: bytes=%d err=%v", len(body), err)
				}
			}
			wantBatches := 3
			if compressed {
				wantBatches = 1
			}
			if len(tx.batches) != wantBatches {
				t.Fatalf("batches=%d want=%d", len(tx.batches), wantBatches)
			}
			for _, batch := range tx.batches {
				if batch.bytes > ownedDocPrefetchBytesPG {
					t.Fatalf("prefetched %d bytes", batch.bytes)
				}
			}
		})
	}
}

func TestPGOwnedDocChunkPrefetchLargeSingleton(t *testing.T) {
	storage, ctx, tx, repo := ownedDocPrefetchPG(t)
	var chunks []domain.ContentHash
	sizes := []int{domain.ChunkTarget, ownedDocPrefetchBytesPG + domain.ChunkTarget, domain.ChunkTarget}
	for ordinal, size := range sizes {
		body := bytes.Repeat([]byte{byte(ordinal + 1)}, size)
		chunks = append(chunks, putOwnedDocPrefetchChunkPG(t, ctx, tx, repo, body, body))
	}
	read := storage.ownedDocChunkReader(repo, chunks)
	for ordinal, hash := range chunks {
		body, err := read(ctx, hash)
		if err != nil || len(body) != sizes[ordinal] || domain.HashContent(body) != hash {
			t.Fatalf("singleton %d: bytes=%d err=%v", ordinal, len(body), err)
		}
	}
	if len(tx.batches) != len(sizes) {
		t.Fatalf("batches=%d want=%d", len(tx.batches), len(sizes))
	}
	for ordinal, batch := range tx.batches {
		if batch.rows != 1 || batch.bytes != sizes[ordinal] {
			t.Fatalf("batch %d: %+v", ordinal, batch)
		}
	}
}

func TestPGOwnedDocChunkPrefetchOversizeBeforeAllocation(t *testing.T) {
	storage, ctx, tx, repo := ownedDocPrefetchPG(t)
	size := ownedDocChunkBytesPG + 1
	digest := sha256.New()
	part := bytes.Repeat([]byte("x"), 64<<10)
	for remaining := size; remaining > 0; remaining -= min(remaining, len(part)) {
		_, _ = digest.Write(part[:min(remaining, len(part))])
	}
	hash := domain.ContentHash(fmt.Sprintf("sha256:%x", digest.Sum(nil)))
	if _, err := tx.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,convert_to(repeat('x',$2),'UTF8'))`, string(hash), size); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, string(repo), string(hash)); err != nil {
		t.Fatal(err)
	}
	for _, chunks := range [][]domain.ContentHash{{hash}, nil, make([]domain.ContentHash, domain.MaxDocJobChunks+1)} {
		body, err := storage.ownedDocChunkReader(repo, chunks)(ctx, hash)
		if !errors.Is(err, domain.ErrDocumentResourceLimit) || errors.Is(err, domain.ErrIntegrity) || len(body) != 0 {
			t.Fatalf("oversized valid chunk: bytes=%d err=%v", len(body), err)
		}
	}
	for _, batch := range tx.batches {
		if batch.rows != 1 || batch.bytes != 0 {
			t.Fatalf("oversized body reached client: %+v", batch)
		}
	}
}

func TestPGOwnedDocChunkPrefetchOwnershipFailureAndCancellation(t *testing.T) {
	storage, ctx, tx, repo := ownedDocPrefetchPG(t)
	first := putOwnedDocPrefetchChunkPG(t, ctx, tx, repo, []byte("first"), []byte("first"))
	otherRepo := domain.HashContent([]byte("foreign"))
	second := putOwnedDocPrefetchChunkPG(t, ctx, tx, otherRepo, []byte("second"), []byte("second"))
	read := storage.ownedDocChunkReader(repo, []domain.ContentHash{first, second, first})
	if _, err := read(ctx, first); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unowned prefetch: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, string(repo), string(second)); err != nil {
		t.Fatal(err)
	}
	if body, err := read(ctx, first); err != nil || string(body) != "first" || len(tx.batches) != 2 {
		t.Fatalf("partial window survived failure: body=%q batches=%d err=%v", body, len(tx.batches), err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if body, err := read(cancelled, second); !errors.Is(err, context.Canceled) || len(body) != 0 {
		t.Fatalf("cancelled cache hit: bytes=%d err=%v", len(body), err)
	}
	if body, err := read(ctx, second); err != nil || string(body) != "second" || len(tx.batches) != 2 {
		t.Fatalf("window reuse: body=%q batches=%d err=%v", body, len(tx.batches), err)
	}
	if body, err := read(ctx, first); err != nil || string(body) != "first" || len(tx.batches) != 3 {
		t.Fatalf("repeat read: body=%q batches=%d err=%v", body, len(tx.batches), err)
	}
	if body, err := storage.ownedDocChunkReader(otherRepo, []domain.ContentHash{first})(ctx, first); !errors.Is(err, domain.ErrNotFound) || len(body) != 0 {
		t.Fatalf("foreign reader: bytes=%d err=%v", len(body), err)
	}
}
