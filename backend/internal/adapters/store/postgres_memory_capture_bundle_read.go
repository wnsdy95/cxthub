//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const memoryCapturePreflightSQL = `WITH bounded AS MATERIALIZED (
	SELECT part.ordinal,part.source_seq,part.block_hash,
		octet_length(part.kind) AS kind_bytes,octet_length(part.entity_key) AS key_bytes,
		octet_length(block.body) AS body_bytes
	FROM memory_capture_parts part LEFT JOIN memory_capture_blocks block
		ON block.repo_id=part.repo_id AND block.block_hash=part.block_hash
	WHERE part.repo_id=$1 AND part.capture_id=$2 ORDER BY part.ordinal LIMIT $3)
	SELECT count(*),COALESCE(sum(body_bytes),0),COALESCE(sum(kind_bytes::bigint+key_bytes),0),
		COALESCE(max(body_bytes),0),COALESCE(max(key_bytes),0),COALESCE(max(kind_bytes),0),
		COALESCE(min(ordinal),-1),COALESCE(max(ordinal),-1),
		COALESCE(bool_or(body_bytes IS NULL OR source_seq<0 OR kind_bytes<1 OR key_bytes<1
			OR block_hash !~ '^sha256:[0-9a-f]{64}$'),false) FROM bounded`

const memoryCapturePartsSQL = `WITH measured AS (
	SELECT part.ordinal,part.kind,part.entity_key,part.source_seq,part.block_hash,block.body,
		octet_length(part.kind) AS kind_bytes,octet_length(part.entity_key) AS key_bytes,
		octet_length(block.body) AS body_bytes,
		sum(octet_length(block.body)::bigint+octet_length(part.kind)+octet_length(part.entity_key)+128)
			OVER (ORDER BY part.ordinal ROWS UNBOUNDED PRECEDING) AS running_bytes
	FROM memory_capture_parts part LEFT JOIN memory_capture_blocks block
		ON block.repo_id=part.repo_id AND block.block_hash=part.block_hash
	WHERE part.repo_id=$1 AND part.capture_id=$2)
	SELECT ordinal,source_seq,body_bytes,kind_bytes,key_bytes,
		CASE WHEN octet_length(block_hash)=71 THEN block_hash END,
		CASE WHEN kind_bytes<=$6 AND running_bytes<=$7 THEN kind END,
		CASE WHEN key_bytes<=$5 AND running_bytes<=$7 THEN entity_key END,
		CASE WHEN body_bytes<=$4 AND kind_bytes<=$6 AND key_bytes<=$5 AND running_bytes<=$7 THEN body END
	FROM measured ORDER BY ordinal LIMIT $3`

func (storage *PostgresStore) ReadMemoryCapture(ctx context.Context, repo, identifier domain.ContentHash) (domain.MemoryCaptureBundle, error) {
	if err := ctx.Err(); err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	if err := validateHashes(repo, identifier); err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	if storage == nil || storage.pool == nil {
		return domain.MemoryCaptureBundle{}, domain.ErrConflict
	}
	if prior, present := ctx.Value(repositoryTxKey{}).(*repositoryTx); present {
		if prior == nil || prior.owner != storage || !prior.readOnly || prior.Tx == nil || (prior.repo != "" && prior.repo != repo) {
			return domain.MemoryCaptureBundle{}, domain.ErrConflict
		}
		return readMemoryCaptureBundle(ctx, prior.Tx, repo, identifier)
	}
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	defer rollbackPG(transaction)
	bundle, err := readMemoryCaptureBundle(ctx, transaction, repo, identifier)
	if err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	return bundle, nil
}

func readMemoryCaptureBundle(ctx context.Context, transaction pgx.Tx, repo, identifier domain.ContentHash) (domain.MemoryCaptureBundle, error) {
	stored, err := readMemoryCaptureHeader(ctx, transaction, repo, identifier, false)
	if err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	header, count, bodyBytes, keyBytes := stored.raw, stored.partCount, stored.bodyBytes, stored.keyBytes
	headerBytes := int64(len(header))
	var actualCount, actualBody, actualKeys, maxBody, maxKey, maxKind, first, last int64
	var invalid bool
	if err := transaction.QueryRow(ctx, memoryCapturePreflightSQL, string(repo), string(identifier), domain.MaxMemoryCaptureParts+1).
		Scan(&actualCount, &actualBody, &actualKeys, &maxBody, &maxKey, &maxKind, &first, &last, &invalid); err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	if actualCount > domain.MaxMemoryCaptureParts || maxBody > domain.MaxMemoryCapturePartBytes ||
		maxKey > memoryCaptureKeyBytes || maxKind > memoryCaptureKindBytes || actualBody+actualKeys+headerBytes+actualCount*128 > domain.MaxMemoryCaptureBytes {
		return domain.MemoryCaptureBundle{}, domain.ErrMemoryProjectionLimit
	}
	if invalid || actualCount != count || actualBody != bodyBytes || actualKeys != keyBytes || first != 0 || last != count-1 || int64(len(header)) != headerBytes {
		return domain.MemoryCaptureBundle{}, domain.ErrIntegrity
	}
	bundle := domain.MemoryCaptureBundle{ID: identifier, Header: stored.header}
	if err := memoryCaptureCheckAnchor(ctx, transaction, &stored, false); err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	rows, err := transaction.Query(ctx, memoryCapturePartsSQL, string(repo), string(identifier), domain.MaxMemoryCaptureParts+1,
		domain.MaxMemoryCapturePartBytes, memoryCaptureKeyBytes, memoryCaptureKindBytes, domain.MaxMemoryCaptureBytes-headerBytes)
	if err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	defer rows.Close()
	bundle.Parts = make([]domain.MemoryCapturePart, 0, count)
	var transferred int64
	for rows.Next() {
		var part domain.MemoryCapturePart
		var ordinal, bodySize, kindSize, keySize int64
		var digest, kind, key *string
		if err := rows.Scan(&ordinal, &part.Sequence, &bodySize, &kindSize, &keySize, &digest, &kind, &key, &part.Value); err != nil {
			return domain.MemoryCaptureBundle{}, err
		}
		if err := ctx.Err(); err != nil {
			return domain.MemoryCaptureBundle{}, err
		}
		if ordinal != int64(len(bundle.Parts)) || ordinal >= count || digest == nil || kind == nil || key == nil || part.Value == nil ||
			int64(len(part.Value)) != bodySize || int64(len(*kind)) != kindSize || int64(len(*key)) != keySize || domain.HashContent(part.Value) != domain.ContentHash(*digest) {
			return domain.MemoryCaptureBundle{}, domain.ErrIntegrity
		}
		transferred += bodySize + kindSize + keySize
		if transferred+headerBytes+(ordinal+1)*128 > domain.MaxMemoryCaptureBytes {
			return domain.MemoryCaptureBundle{}, domain.ErrMemoryProjectionLimit
		}
		part.Kind, part.Key = *kind, *key
		bundle.Parts = append(bundle.Parts, part)
	}
	if err := rows.Err(); err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	rows.Close()
	if int64(len(bundle.Parts)) != count || transferred != bodyBytes+keyBytes {
		return domain.MemoryCaptureBundle{}, domain.ErrIntegrity
	}
	if err := domain.ValidateMemoryCapture(ctx, bundle); err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	base, err := domain.MemoryCaptureGraphBase(bundle)
	if err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	if base.ID != stored.anchorID || base.Checkpoint != stored.checkpoint {
		return domain.MemoryCaptureBundle{}, domain.ErrIntegrity
	}
	if err := memoryCaptureCheckExpiry(ctx, transaction, repo, identifier); err != nil {
		return domain.MemoryCaptureBundle{}, err
	}
	return bundle, nil
}
