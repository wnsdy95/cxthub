//go:build postgres

package store

import (
	"bytes"
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const (
	memoryReadBatchRows     = 64
	memoryReadBatchBytes    = 2 << 20
	memoryReadBatchOverhead = 1024
)

var _ outbound.MemoryReadStore = (*PostgresStore)(nil)

func (storage *PostgresStore) WriteMemoryRead(ctx context.Context, bundle domain.MemoryReadBundle) error {
	if err := storage.memoryCaptureOwnWrite(ctx); err != nil {
		return err
	}
	if err := domain.ValidateMemoryReadBundle(ctx, bundle); err != nil {
		return err
	}
	header, err := domain.MemoryReadHeaderBytes(ctx, bundle.Header)
	if err != nil {
		return err
	}
	if domain.HashContent(header) != bundle.ID {
		return domain.ErrIntegrity
	}
	repo := bundle.Header.Source.Graph.RepoID
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if err := lockMemoryCaptureRepo(ctx, transaction, repo); err != nil {
		return err
	}
	if err := memoryReadCaptureHeader(ctx, transaction, bundle.Header, true); err != nil {
		return err
	}
	var exists bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memory_read_headers WHERE repo_id=$1 AND read_id=$2)`, string(repo), string(bundle.ID)).Scan(&exists); err != nil {
		return err
	}
	if exists {
		stored, err := readMemoryReadHeader(ctx, transaction, repo, bundle.ID)
		if err != nil {
			return err
		}
		storedBytes, err := domain.MemoryReadHeaderBytes(ctx, stored)
		if err != nil {
			return err
		}
		if !bytes.Equal(storedBytes, header) {
			return domain.ErrIntegrity
		}
	}
	var bodyBytes int64
	for start := 0; start < len(bundle.Chunks); {
		end := memoryReadBatchEnd(bundle.Chunks, start)
		if err := writeMemoryReadBatch(ctx, transaction, bundle, start, end, exists); err != nil {
			return err
		}
		for ordinal := start; ordinal < end; ordinal++ {
			bodyBytes += int64(len(bundle.Chunks[ordinal].Bytes))
		}
		start = end
	}
	if !exists {
		if _, err := transaction.Exec(ctx, `INSERT INTO memory_read_headers
			(repo_id,read_id,capture_id,header,chunk_count,item_count,body_bytes) VALUES($1,$2,$3,$4,$5,$6,$7)`,
			string(repo), string(bundle.ID), string(bundle.Header.CaptureID), header, len(bundle.Chunks), bundle.Header.View.Total, bodyBytes); err != nil {
			return err
		}
	}
	if err := memoryReadCheckExpiry(ctx, transaction, repo, bundle.Header.CaptureID); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func memoryReadBatchEnd(chunks []domain.MemoryReadChunk, start int) int {
	end, size := start, 0
	for end < len(chunks) && end-start < memoryReadBatchRows {
		chunkBytes := len(chunks[end].Bytes) + memoryReadBatchOverhead
		if end > start && size+chunkBytes > memoryReadBatchBytes {
			break
		}
		size += chunkBytes
		end++
	}
	return end
}

func writeMemoryReadBatch(ctx context.Context, transaction pgx.Tx, bundle domain.MemoryReadBundle, start, end int, exists bool) error {
	batch := &pgx.Batch{}
	for ordinal := start; ordinal < end; ordinal++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, reference := bundle.Chunks[ordinal], bundle.Header.Chunks[ordinal]
		query := `INSERT INTO memory_read_chunks(repo_id,read_id,ordinal,chunk_hash,start_item,item_count,body) VALUES($1,$2,$3,$4,$5,$6,$7)`
		if exists {
			query = `SELECT EXISTS(SELECT 1 FROM memory_read_chunks WHERE repo_id=$1 AND read_id=$2 AND ordinal=$3
				AND chunk_hash=$4 AND start_item=$5 AND item_count=$6 AND body=$7)`
		}
		batch.Queue(query, string(bundle.Header.Source.Graph.RepoID), string(bundle.ID), ordinal, string(chunk.Hash), reference.Start, reference.Count, chunk.Bytes)
	}
	results := transaction.SendBatch(ctx, batch)
	defer results.Close()
	for ordinal := start; ordinal < end; ordinal++ {
		if exists {
			var matches bool
			if err := results.QueryRow().Scan(&matches); err != nil {
				return err
			}
			if !matches {
				return domain.ErrIntegrity
			}
		} else if _, err := results.Exec(); err != nil {
			return err
		}
	}
	return results.Close()
}
