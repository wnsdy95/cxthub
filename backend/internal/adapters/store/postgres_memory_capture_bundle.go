//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const (
	memoryCaptureHeaderBytes   = 16 << 10
	memoryCaptureKindBytes     = 64
	memoryCaptureKeyBytes      = 4096
	memoryCaptureBatchParts    = 64
	memoryCaptureBatchBytes    = 2 << 20
	memoryCaptureBatchOverhead = 1024
	memoryCaptureRetention     = 24 * time.Hour
	memoryCapturePruneLimit    = 32
)

var _ outbound.MemoryCaptureStore = (*PostgresStore)(nil)

func memoryCaptureBundleSizes(ctx context.Context, bundle domain.MemoryCaptureBundle) ([]byte, int64, int64, error) {
	if err := domain.ValidateMemoryCapture(ctx, bundle); err != nil {
		return nil, 0, 0, err
	}
	header, err := json.Marshal(bundle.Header)
	if err != nil {
		return nil, 0, 0, err
	}
	if len(header) > memoryCaptureHeaderBytes || len(bundle.Parts) > domain.MaxMemoryCaptureParts {
		return nil, 0, 0, domain.ErrMemoryProjectionLimit
	}
	var bodyBytes, keyBytes int64
	for _, part := range bundle.Parts {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		if len(part.Kind) > memoryCaptureKindBytes || len(part.Key) > memoryCaptureKeyBytes || len(part.Value) > domain.MaxMemoryCapturePartBytes {
			return nil, 0, 0, domain.ErrMemoryProjectionLimit
		}
		if part.Kind == "" || part.Key == "" || part.Sequence < 0 || !utf8.ValidString(part.Kind) || !utf8.ValidString(part.Key) || strings.ContainsRune(part.Key, 0) || strings.ContainsRune(part.Kind, 0) {
			return nil, 0, 0, domain.ErrIntegrity
		}
		bodyBytes += int64(len(part.Value))
		keyBytes += int64(len(part.Kind) + len(part.Key))
	}
	if bodyBytes+keyBytes+int64(len(header))+int64(len(bundle.Parts))*128 > domain.MaxMemoryCaptureBytes {
		return nil, 0, 0, domain.ErrMemoryProjectionLimit
	}
	return header, bodyBytes, keyBytes, nil
}

func (storage *PostgresStore) memoryCaptureOwnWrite(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if storage == nil || storage.pool == nil || storage.InTransaction(ctx) {
		return domain.ErrConflict
	}
	return nil
}

func lockMemoryCaptureRepo(ctx context.Context, transaction pgx.Tx, repo domain.ContentHash) error {
	if _, err := transaction.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cxt-memory-capture:' || $1,0))`, string(repo)); err != nil {
		return err
	}
	var owner string
	return mapNoRows(transaction.QueryRow(ctx, `SELECT id FROM repos WHERE id=$1 FOR KEY SHARE`, string(repo)).Scan(&owner))
}

func (storage *PostgresStore) WriteMemoryCapture(ctx context.Context, bundle domain.MemoryCaptureBundle, expiresAt time.Time) error {
	if err := storage.memoryCaptureOwnWrite(ctx); err != nil {
		return err
	}
	expiresAt = expiresAt.UTC().Truncate(time.Microsecond)
	now := time.Now()
	if !expiresAt.After(now) || expiresAt.After(now.Add(memoryCaptureRetention)) {
		return domain.ErrValidation
	}
	header, bodyBytes, keyBytes, err := memoryCaptureBundleSizes(ctx, bundle)
	if err != nil {
		return err
	}
	base, err := domain.MemoryCaptureGraphBase(bundle)
	if err != nil {
		return err
	}
	repo := bundle.Header.Generation.Graph.RepoID
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if err := lockMemoryCaptureRepo(ctx, transaction, repo); err != nil {
		return err
	}
	var exists bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2)`, string(repo), string(bundle.ID)).Scan(&exists); err != nil {
		return err
	}
	if exists {
		if _, err := readMemoryCaptureBundle(ctx, transaction, repo, bundle.ID); err != nil {
			return err
		}
		return transaction.Commit(ctx)
	}
	stored := memoryCaptureStoredHeader{header: bundle.Header, expiresAt: expiresAt, anchorID: base.ID}
	if err := memoryCaptureCheckAnchor(ctx, transaction, &stored, true); err != nil {
		return err
	}
	if stored.checkpoint != base.Checkpoint {
		return domain.ErrIntegrity
	}
	for start := 0; start < len(bundle.Parts); {
		end := memoryCaptureBatchEnd(bundle.Parts, start)
		if err := writeMemoryCaptureBatch(ctx, transaction, bundle, start, end); err != nil {
			return err
		}
		start = end
	}
	if err := transaction.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if !expiresAt.After(now) || expiresAt.After(now.Add(memoryCaptureRetention)) {
		return domain.ErrValidation
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO memory_capture_headers
		(repo_id,capture_id,header,part_count,body_bytes,key_bytes,complete,created_at,expires_at,
		capture_version,graph_epoch,graph_sequence,graph_anchor_id)
		VALUES($1,$2,$3,$4,$5,$6,true,$7,$8,$9,$10,$11,NULLIF($12,''))`, string(repo), string(bundle.ID), header,
		len(bundle.Parts), bodyBytes, keyBytes, now, expiresAt, bundle.Header.Version, bundle.Header.Generation.Graph.Epoch,
		bundle.Header.Generation.Graph.Sequence, string(base.ID)); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func memoryCaptureBatchEnd(parts []domain.MemoryCapturePart, start int) int {
	end, size := start, 0
	for end < len(parts) && end-start < memoryCaptureBatchParts {
		partBytes := len(parts[end].Value) + len(parts[end].Kind) + len(parts[end].Key) + memoryCaptureBatchOverhead
		if end > start && size+partBytes > memoryCaptureBatchBytes {
			break
		}
		size += partBytes
		end++
	}
	return end
}

func writeMemoryCaptureBatch(ctx context.Context, transaction pgx.Tx, bundle domain.MemoryCaptureBundle, start, end int) error {
	repo := string(bundle.Header.Generation.Graph.RepoID)
	batch := &pgx.Batch{}
	for ordinal := start; ordinal < end; ordinal++ {
		part := bundle.Parts[ordinal]
		digest := string(domain.HashContent(part.Value))
		body := part.Value
		if body == nil {
			body = []byte{}
		}
		batch.Queue(`WITH inserted AS (
			INSERT INTO memory_capture_blocks(repo_id,block_hash,body) VALUES($1,$2,$3)
			ON CONFLICT(repo_id,block_hash) DO NOTHING RETURNING 1)
			SELECT EXISTS(SELECT 1 FROM inserted) OR EXISTS(
			SELECT 1 FROM memory_capture_blocks WHERE repo_id=$1 AND block_hash=$2 AND body=$3)`, repo, digest, body)
		batch.Queue(`INSERT INTO memory_capture_parts(repo_id,capture_id,ordinal,kind,entity_key,source_seq,block_hash)
			VALUES($1,$2,$3,$4,$5,$6,$7)`, repo, string(bundle.ID), ordinal, part.Kind, part.Key, part.Sequence, digest)
	}
	results := transaction.SendBatch(ctx, batch)
	defer results.Close()
	for ordinal := start; ordinal < end; ordinal++ {
		var matches bool
		if err := results.QueryRow().Scan(&matches); err != nil {
			return err
		}
		if !matches {
			return domain.ErrIntegrity
		}
		if _, err := results.Exec(); err != nil {
			return err
		}
	}
	return results.Close()
}

func (storage *PostgresStore) PruneMemoryCaptures(ctx context.Context, repo domain.ContentHash, limit int) (int, error) {
	if err := storage.memoryCaptureOwnWrite(ctx); err != nil {
		return 0, err
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return 0, err
	}
	if limit <= 0 || limit > memoryCapturePruneLimit {
		return 0, domain.ErrValidation
	}
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer rollbackPG(transaction)
	if err := lockMemoryCaptureRepo(ctx, transaction, repo); err != nil {
		return 0, err
	}
	removed := 0
	for removed < limit {
		var identifier string
		err := transaction.QueryRow(ctx, `SELECT capture_id FROM memory_capture_headers capture
			WHERE repo_id=$1 AND expires_at<=clock_timestamp()
			AND NOT EXISTS(SELECT 1 FROM memory_capture_headers child
				WHERE child.repo_id=capture.repo_id AND child.graph_anchor_id=capture.capture_id)
			ORDER BY expires_at,capture_id LIMIT 1 FOR UPDATE`, string(repo)).Scan(&identifier)
		if err == pgx.ErrNoRows {
			break
		}
		if err != nil {
			return 0, err
		}
		rows, err := transaction.Query(ctx, `SELECT block_hash FROM memory_capture_parts
			WHERE repo_id=$1 AND capture_id=$2 ORDER BY ordinal LIMIT $3`, string(repo), identifier, domain.MaxMemoryCaptureParts+1)
		if err != nil {
			return 0, err
		}
		hashes, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return 0, err
		}
		if len(hashes) > domain.MaxMemoryCaptureParts {
			return 0, domain.ErrMemoryProjectionLimit
		}
		if _, err := transaction.Exec(ctx, `DELETE FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(repo), identifier); err != nil {
			return 0, err
		}
		for start := 0; start < len(hashes); start += memoryCaptureBatchParts {
			if _, err := transaction.Exec(ctx, `DELETE FROM memory_capture_blocks block WHERE block.repo_id=$1
				AND block.block_hash=ANY($2::text[])
				AND NOT EXISTS(SELECT 1 FROM memory_capture_parts part WHERE part.repo_id=block.repo_id AND part.block_hash=block.block_hash)`,
				string(repo), hashes[start:min(start+memoryCaptureBatchParts, len(hashes))]); err != nil {
				return 0, err
			}
		}
		removed++
	}
	if err := transaction.Commit(ctx); err != nil {
		return 0, err
	}
	return removed, nil
}
