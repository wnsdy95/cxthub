//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestMemoryReadBatchBudgets(test *testing.T) {
	for _, size := range []int{0, 1, 20 << 10, domain.MaxMemoryReadChunkBytes} {
		body := make([]byte, size)
		chunks := make([]domain.MemoryReadChunk, 193)
		for ordinal := range chunks {
			chunks[ordinal].Bytes = body
		}
		for start := 0; start < len(chunks); {
			end := memoryReadBatchEnd(chunks, start)
			cost := (end - start) * (size + memoryReadBatchOverhead)
			if end <= start || end-start > 64 || cost > 2<<20 {
				test.Fatalf("batch [%d,%d) cost=%d", start, end, cost)
			}
			if end < len(chunks) && end-start < 64 && cost+size+memoryReadBatchOverhead <= 2<<20 {
				test.Fatal("batch stopped prematurely")
			}
			start = end
		}
	}
}

func TestPGMemoryReadWriteBounds(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	capture, original := memoryReadPGFixture(test, storage, ctx, repo, 1)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	for _, scenario := range []string{"header", "chunk", "chunks", "items", "result", "identity"} {
		test.Run(scenario, func(test *testing.T) {
			bundle := original
			bundle.Header.Chunks = append([]domain.MemoryReadChunkRef(nil), original.Header.Chunks...)
			bundle.Chunks = append([]domain.MemoryReadChunk(nil), original.Chunks...)
			expected := domain.ErrMemoryProjectionLimit
			switch scenario {
			case "header":
				bundle.Header.Origin = strings.Repeat("x", domain.MaxMemoryReadHeaderBytes+1)
			case "chunk":
				bundle.Chunks[0].Bytes = make([]byte, domain.MaxMemoryReadChunkBytes+1)
			case "chunks":
				bundle.Header.Chunks = make([]domain.MemoryReadChunkRef, domain.MaxMemoryReadChunks+1)
			case "items":
				bundle.Header.View.Total = domain.MaxMemoryReadItems + 1
			case "result":
				bundle.Header.Chunks = make([]domain.MemoryReadChunkRef, 256)
				bundle.Header.View.Total = len(bundle.Header.Chunks)
				for ordinal := range bundle.Header.Chunks {
					bundle.Header.Chunks[ordinal] = domain.MemoryReadChunkRef{Hash: original.Chunks[0].Hash, Start: ordinal, Count: 1, Bytes: domain.MaxMemoryReadChunkBytes}
				}
			case "identity":
				bundle.ID = domain.HashContent([]byte("wrong identity"))
				expected = domain.ErrIntegrity
			}
			if err := storage.WriteMemoryRead(ctx, bundle); !errors.Is(err, expected) {
				test.Fatal("write bound bypassed", err)
			}
			if counts := memoryReadPGCounts(test, storage, ctx, repo); counts != [2]int{} {
				test.Fatal("invalid bundle published", counts)
			}
		})
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := storage.WriteMemoryRead(canceled, original); !errors.Is(err, context.Canceled) {
		test.Fatal("canceled write", err)
	}
	if _, err := storage.ReadMemoryReadHeader(canceled, repo, original.ID); !errors.Is(err, context.Canceled) {
		test.Fatal("canceled header", err)
	}
	if _, err := storage.ReadMemoryReadChunk(canceled, repo, original.ID, 0); !errors.Is(err, context.Canceled) {
		test.Fatal("canceled chunk", err)
	}
}

func TestPGMemoryReadMaximumChunkAndBoundedTransfer(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	capture, original := memoryReadPGFixture(test, storage, ctx, repo, 1)
	items, err := domain.DecodeMemoryReadChunk(ctx, original.Header, 0, original.Chunks[0].Bytes)
	if err != nil {
		test.Fatal(err)
	}
	items[0].Text = ""
	raw, err := json.Marshal(items[0])
	if err != nil {
		test.Fatal(err)
	}
	items[0].Text = strings.Repeat("x", domain.MaxMemoryReadChunkBytes-2-len(raw))
	bundle := memoryReadPGSeal(test, ctx, original.Header, items)
	if len(bundle.Chunks[0].Bytes) != domain.MaxMemoryReadChunkBytes {
		test.Fatal("maximum chunk fixture wrong")
	}
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	memoryReadPGWrite(test, storage, ctx, bundle)
	memoryReadPGAssert(test, storage, ctx, bundle)
	var size int
	var suppressed []byte
	if err := storage.pool.QueryRow(ctx, memoryReadChunkSQL, string(repo), string(bundle.ID), 0, domain.MaxMemoryReadChunkBytes-1).Scan(&size, &suppressed); err != nil {
		test.Fatal(err)
	}
	if size != domain.MaxMemoryReadChunkBytes || suppressed != nil {
		test.Fatal("oversized chunk transferred before validation")
	}
	if actual, err := storage.ReadMemoryReadChunk(ctx, repo, bundle.ID, 0); err != nil || !bytes.Equal(actual, bundle.Chunks[0].Bytes) {
		test.Fatal("bounded chunk damaged", err)
	}
}

func TestPGMemoryReadBoundedHeaderPreflight(test *testing.T) {
	for _, scenario := range []string{"header", "chunk", "chunks", "items", "result", "capture-header", "negative"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 1)
			memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
			memoryReadPGWrite(test, storage, ctx, bundle)
			transaction := catalogTxPG(test, ctx, storage)
			defer rollbackPG(transaction)
			for _, table := range []string{"memory_read_headers", "memory_read_chunks", "memory_capture_headers"} {
				rows, err := transaction.Query(ctx, `SELECT conname FROM pg_constraint WHERE conrelid=$1::regclass AND contype='c'`, table)
				if err != nil {
					test.Fatal(err)
				}
				names, err := pgx.CollectRows(rows, pgx.RowTo[string])
				if err != nil {
					test.Fatal(err)
				}
				for _, name := range names {
					catalogExecPG(test, ctx, transaction, `ALTER TABLE `+pgx.Identifier{table}.Sanitize()+` DROP CONSTRAINT `+pgx.Identifier{name}.Sanitize())
				}
			}
			catalogExecPG(test, ctx, transaction, `SET LOCAL session_replication_role=replica`)
			expected := domain.ErrMemoryProjectionLimit
			switch scenario {
			case "header":
				catalogExecPG(test, ctx, transaction, `UPDATE memory_read_headers SET header=$2 WHERE repo_id=$1`, string(repo), bytes.Repeat([]byte("x"), domain.MaxMemoryReadHeaderBytes+1))
			case "chunk":
				catalogExecPG(test, ctx, transaction, `UPDATE memory_read_chunks SET body=$2 WHERE repo_id=$1`, string(repo), bytes.Repeat([]byte("x"), domain.MaxMemoryReadChunkBytes+1))
			case "chunks":
				catalogExecPG(test, ctx, transaction, `UPDATE memory_read_headers SET chunk_count=1025 WHERE repo_id=$1`, string(repo))
			case "items":
				catalogExecPG(test, ctx, transaction, `UPDATE memory_read_headers SET item_count=16385 WHERE repo_id=$1`, string(repo))
			case "result":
				catalogExecPG(test, ctx, transaction, `UPDATE memory_read_headers SET body_bytes=67108865 WHERE repo_id=$1`, string(repo))
			case "capture-header":
				catalogExecPG(test, ctx, transaction, `UPDATE memory_capture_headers SET header=$2 WHERE repo_id=$1`, string(repo), bytes.Repeat([]byte("x"), memoryCaptureHeaderBytes+1))
			case "negative":
				catalogExecPG(test, ctx, transaction, `UPDATE memory_read_headers SET body_bytes=-1 WHERE repo_id=$1`, string(repo))
				expected = domain.ErrIntegrity
			}
			if header, err := readMemoryReadHeader(ctx, transaction, repo, bundle.ID); !errors.Is(err, expected) || header.CaptureID != "" {
				test.Fatal("bounded metadata preflight failed", err)
			}
		})
	}
}
