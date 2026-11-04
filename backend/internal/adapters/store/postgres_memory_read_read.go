//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const memoryReadIndexSQL = `SELECT ordinal,CASE WHEN octet_length(chunk_hash)=71 THEN chunk_hash END,
	start_item,item_count,octet_length(body) FROM memory_read_chunks
	WHERE repo_id=$1 AND read_id=$2 ORDER BY ordinal LIMIT $3`

const memoryReadChunkSQL = `SELECT octet_length(body),CASE WHEN octet_length(body)<=$4 THEN body END
	FROM memory_read_chunks WHERE repo_id=$1 AND read_id=$2 AND ordinal=$3`

func (storage *PostgresStore) memoryReadSnapshot(ctx context.Context, repo, identifier domain.ContentHash) (pgx.Tx, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := validateHashes(repo, identifier); err != nil {
		return nil, false, err
	}
	if storage == nil || storage.pool == nil {
		return nil, false, domain.ErrConflict
	}
	if prior, present := ctx.Value(repositoryTxKey{}).(*repositoryTx); present {
		if prior == nil || prior.owner != storage || !prior.readOnly || prior.Tx == nil || (prior.repo != "" && prior.repo != repo) {
			return nil, false, domain.ErrConflict
		}
		return prior.Tx, false, nil
	}
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	return transaction, true, err
}

func (storage *PostgresStore) ReadMemoryReadHeader(ctx context.Context, repo, identifier domain.ContentHash) (domain.MemoryReadHeader, error) {
	transaction, owned, err := storage.memoryReadSnapshot(ctx, repo, identifier)
	if err != nil {
		return domain.MemoryReadHeader{}, err
	}
	if owned {
		defer rollbackPG(transaction)
	}
	header, err := readMemoryReadHeader(ctx, transaction, repo, identifier)
	if err != nil {
		return domain.MemoryReadHeader{}, err
	}
	if err := memoryReadCheckExpiry(ctx, transaction, repo, header.CaptureID); err != nil {
		return domain.MemoryReadHeader{}, err
	}
	if owned {
		if err := transaction.Commit(ctx); err != nil {
			return domain.MemoryReadHeader{}, err
		}
	}
	return header, nil
}

func (storage *PostgresStore) ReadMemoryReadChunk(ctx context.Context, repo, identifier domain.ContentHash, ordinal int) ([]byte, error) {
	if ordinal < 0 || ordinal >= domain.MaxMemoryReadChunks {
		return nil, domain.ErrValidation
	}
	transaction, owned, err := storage.memoryReadSnapshot(ctx, repo, identifier)
	if err != nil {
		return nil, err
	}
	if owned {
		defer rollbackPG(transaction)
	}
	header, err := readMemoryReadHeader(ctx, transaction, repo, identifier)
	if err != nil {
		return nil, err
	}
	if ordinal >= len(header.Chunks) {
		return nil, domain.ErrValidation
	}
	var raw []byte
	var size int64
	if err := transaction.QueryRow(ctx, memoryReadChunkSQL, string(repo), string(identifier), ordinal, domain.MaxMemoryReadChunkBytes).Scan(&size, &raw); err != nil {
		return nil, mapNoRows(err)
	}
	if size > domain.MaxMemoryReadChunkBytes {
		return nil, domain.ErrMemoryProjectionLimit
	}
	if int64(len(raw)) != size {
		return nil, domain.ErrIntegrity
	}
	if _, err := domain.DecodeMemoryReadChunk(ctx, header, ordinal, raw); err != nil {
		return nil, err
	}
	if err := memoryReadCheckExpiry(ctx, transaction, repo, header.CaptureID); err != nil {
		return nil, err
	}
	if owned {
		if err := transaction.Commit(ctx); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

func readMemoryReadHeader(ctx context.Context, transaction pgx.Tx, repo, identifier domain.ContentHash) (domain.MemoryReadHeader, error) {
	header, _, err := readMemoryReadDescriptor(ctx, transaction, repo, identifier)
	if err != nil {
		return domain.MemoryReadHeader{}, err
	}
	rows, err := transaction.Query(ctx, memoryReadIndexSQL, string(repo), string(identifier), domain.MaxMemoryReadChunks+1)
	if err != nil {
		return domain.MemoryReadHeader{}, err
	}
	defer rows.Close()
	actualCount := 0
	for rows.Next() {
		var ordinal, start, count, size int64
		var digest *string
		if err := rows.Scan(&ordinal, &digest, &start, &count, &size); err != nil {
			return domain.MemoryReadHeader{}, err
		}
		if err := ctx.Err(); err != nil {
			return domain.MemoryReadHeader{}, err
		}
		if actualCount >= domain.MaxMemoryReadChunks || size > domain.MaxMemoryReadChunkBytes {
			return domain.MemoryReadHeader{}, domain.ErrMemoryProjectionLimit
		}
		if ordinal != int64(actualCount) || actualCount >= len(header.Chunks) || digest == nil {
			return domain.MemoryReadHeader{}, domain.ErrIntegrity
		}
		reference := header.Chunks[actualCount]
		if domain.ContentHash(*digest) != reference.Hash || start != int64(reference.Start) || count != int64(reference.Count) || size != int64(reference.Bytes) {
			return domain.MemoryReadHeader{}, domain.ErrIntegrity
		}
		actualCount++
	}
	if err := rows.Err(); err != nil {
		return domain.MemoryReadHeader{}, err
	}
	if actualCount != len(header.Chunks) {
		return domain.MemoryReadHeader{}, domain.ErrIntegrity
	}
	return header, nil
}

func readMemoryReadDescriptor(ctx context.Context, transaction pgx.Tx, repo, identifier domain.ContentHash) (domain.MemoryReadHeader, *domain.MemoryReadChunkVerifier, error) {
	var raw []byte
	var capture *string
	var headerBytes, chunkCount, itemCount, bodyBytes int64
	err := transaction.QueryRow(ctx, `SELECT octet_length(header),chunk_count,item_count,body_bytes,
		CASE WHEN octet_length(capture_id)=71 THEN capture_id END,
		CASE WHEN octet_length(header)<=$3 THEN header END
		FROM memory_read_headers WHERE repo_id=$1 AND read_id=$2`, string(repo), string(identifier), domain.MaxMemoryReadHeaderBytes).
		Scan(&headerBytes, &chunkCount, &itemCount, &bodyBytes, &capture, &raw)
	if err != nil {
		return domain.MemoryReadHeader{}, nil, mapNoRows(err)
	}
	if headerBytes > domain.MaxMemoryReadHeaderBytes || chunkCount > domain.MaxMemoryReadChunks || itemCount > domain.MaxMemoryReadItems ||
		bodyBytes > domain.MaxMemoryReadBytes || bodyBytes+headerBytes > domain.MaxMemoryReadBytes {
		return domain.MemoryReadHeader{}, nil, domain.ErrMemoryProjectionLimit
	}
	if headerBytes < 1 || chunkCount < 0 || itemCount < 0 || bodyBytes < 0 || capture == nil || int64(len(raw)) != headerBytes {
		return domain.MemoryReadHeader{}, nil, domain.ErrIntegrity
	}
	if domain.HashContent(raw) != identifier {
		return domain.MemoryReadHeader{}, nil, domain.ErrIntegrity
	}
	var header domain.MemoryReadHeader
	if err := domain.DecodeRepositorySourceJSON(ctx, raw, &header); err != nil {
		return domain.MemoryReadHeader{}, nil, err
	}
	verifier, err := domain.NewMemoryReadChunkVerifier(ctx, repo, identifier, header)
	if err != nil {
		return domain.MemoryReadHeader{}, nil, err
	}
	if header.CaptureID != domain.ContentHash(*capture) || int64(len(header.Chunks)) != chunkCount || int64(header.View.Total) != itemCount {
		return domain.MemoryReadHeader{}, nil, domain.ErrIntegrity
	}
	var actualBytes int64
	for _, reference := range header.Chunks {
		actualBytes += int64(reference.Bytes)
	}
	if actualBytes != bodyBytes {
		return domain.MemoryReadHeader{}, nil, domain.ErrIntegrity
	}
	if err := memoryReadCaptureHeader(ctx, transaction, header, false); err != nil {
		return domain.MemoryReadHeader{}, nil, err
	}
	return header, verifier, nil
}

func memoryReadCaptureHeader(ctx context.Context, transaction pgx.Tx, header domain.MemoryReadHeader, lock bool) error {
	stored, err := readMemoryCaptureHeader(ctx, transaction, header.Source.Graph.RepoID, header.CaptureID, lock)
	if err != nil {
		return err
	}
	if stored.header.Generation != header.Source || stored.header.Selection != header.View.Selection || stored.header.Content != header.View.Content {
		return domain.ErrIntegrity
	}
	return memoryCaptureCheckAnchor(ctx, transaction, &stored, lock)
}

func memoryReadCheckExpiry(ctx context.Context, transaction pgx.Tx, repo, capture domain.ContentHash) error {
	return memoryCaptureCheckExpiry(ctx, transaction, repo, capture)
}
