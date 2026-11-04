//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *PostgresStore) readRangeIndex(ctx context.Context, repo, hash domain.ContentHash, position int) (domain.DocRangeIndex, []byte, error) {
	if err := validateHashes(repo, hash); err != nil {
		return domain.DocRangeIndex{}, nil, err
	}
	var root, raw, page []byte
	err := s.db(ctx).QueryRow(ctx, `SELECT substring(b.bytes FROM 1 FOR $4), substring(i.header FROM 1 FOR $5), substring(p.bytes FROM 1 FOR $6)
 FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 LEFT JOIN doc_range_indexes_v1 i ON i.hash=rb.hash
 LEFT JOIN doc_range_pages_v1 p ON p.doc_hash=i.hash AND p.ordinal=$3
 WHERE rb.repo_id=$1 AND rb.kind='doc' AND rb.hash=$2`, repo, hash, position,
		maxStoredRangeRootBytes+1, domain.MaxDocRangeIndexBytes+1, domain.MaxDocRangePageBytes+1).Scan(&root, &raw, &page)
	if err != nil {
		return domain.DocRangeIndex{}, nil, mapNoRows(err)
	}
	if _, err := rangeRootManifest(ctx, hash, root); err != nil {
		return domain.DocRangeIndex{}, nil, err
	}
	if raw == nil {
		return domain.DocRangeIndex{}, nil, domain.ErrDocRangeIndexUnavailable
	}
	header, err := decodeRangeHeader(hash, raw)
	if err != nil {
		return domain.DocRangeIndex{}, nil, err
	}
	return header, page, ctx.Err()
}

func (s *PostgresStore) DocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocRangeIndex, error) {
	header, _, err := s.readRangeIndex(ctx, repo, hash, -1)
	return header, err
}

func (s *PostgresStore) DocRangePage(ctx context.Context, repo domain.ContentHash, header domain.DocRangeIndex, position int) ([]domain.DocRangeEntry, error) {
	current, raw, err := s.readRangeIndex(ctx, repo, header.Hash, position)
	if err != nil {
		return nil, err
	}
	if err := matchRangeHeader(current, header); err != nil {
		return nil, err
	}
	entries, err := current.DecodePage(position, raw)
	if err != nil {
		return nil, err
	}
	return entries, ctx.Err()
}

func (s *PostgresStore) putRangeIndex(ctx context.Context, repo domain.ContentHash, index domain.VerifiedDocRangeIndex) error {
	bound, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || bound.owner != s || bound.readOnly || bound.repo != repo {
		return domain.ErrConflict
	}
	header, raw, err := verifiedRangeHeader(index)
	if err != nil {
		return err
	}
	proof, objects, err := domain.BuildVerifiedDocRangeGeneration(ctx, index)
	if err != nil {
		return err
	}
	descriptor, err := proof.Descriptor().Encode()
	if err != nil {
		return err
	}
	if err := writeRangeGenerationPG(ctx, bound, header.Hash, raw, domain.HashContent(descriptor), descriptor); err != nil {
		return err
	}
	for start := 0; start < len(objects); start += 128 {
		batch := &pgx.Batch{}
		for _, object := range objects[start:min(start+128, len(objects))] {
			if err := ctx.Err(); err != nil {
				return err
			}
			batch.Queue(`INSERT INTO doc_range_objects_v2(doc_hash,hash,bytes) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, header.Hash, object.Hash, object.Bytes)
		}
		if err := bound.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
	}
	return s.putDocVisibleGeneration(ctx, repo, proof, true)
}

func writeRangeGenerationPG(ctx context.Context, transaction *repositoryTx, hash domain.ContentHash, header []byte, generation domain.ContentHash, descriptor []byte) error {
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_range_indexes_v1(hash,header) VALUES($1,$2)
 ON CONFLICT(hash) DO UPDATE SET header=EXCLUDED.header`, hash, header); err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `DELETE FROM doc_range_pages_v1 WHERE doc_hash=$1`, hash); err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `INSERT INTO doc_range_generations_v2(doc_hash,source_version,generation_hash,descriptor)
 SELECT hash,doc_range_source_version,$2,$3 FROM blobs WHERE hash=$1
 ON CONFLICT(doc_hash) DO UPDATE SET source_version=EXCLUDED.source_version,generation_hash=EXCLUDED.generation_hash,descriptor=EXCLUDED.descriptor`, hash, generation, descriptor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	_, err = transaction.Exec(ctx, `DELETE FROM doc_range_objects_v2 WHERE doc_hash=$1`, hash)
	return err
}

func (s *PostgresStore) ensureRangeIndex(ctx context.Context, repo, hash domain.ContentHash) error {
	return s.ensureRangeVersion(ctx, repo, hash, false)
}

func (s *PostgresStore) ensureRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) error {
	return s.ensureRangeVersion(ctx, repo, hash, true)
}

func (s *PostgresStore) ensureRangeVersion(ctx context.Context, repo, hash domain.ContentHash, generation bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bound, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || bound.owner != s || bound.readOnly || bound.repo != repo || outbound.DocReadOnly(ctx) {
		return domain.ErrConflict
	}
	var raw []byte
	if err := s.db(ctx).QueryRow(ctx, `SELECT substring(b.bytes FROM 1 FOR $3) FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind='doc' AND rb.hash=$2 FOR UPDATE OF rb,b`, repo, hash, maxStoredRangeRootBytes+1).Scan(&raw); err != nil {
		return mapNoRows(err)
	}
	manifest, err := rangeRootManifest(ctx, hash, raw)
	if err != nil {
		return err
	}
	if generation {
		var current domain.DocRangeGeneration
		current, err = s.readRangeGeneration(ctx, repo, hash)
		if err == nil {
			if current.Identity() != domain.DocumentIdentityRootV1 {
				return domain.ErrIntegrity
			}
			_, err = s.readVisibleGeneration(ctx, repo, hash)
		}
	} else {
		_, err = s.DocRangeIndex(ctx, repo, hash)
	}
	if !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		return err
	}
	spool, err := newDocRangeSpool()
	if err != nil {
		return err
	}
	defer spool.Close()
	index, err := verifyStoredRangeIndex(ctx, &s.docProofs.rootVerifier, hash, manifest, func(ctx context.Context, chunk domain.ContentHash) ([]byte, error) {
		var body []byte
		err := s.db(ctx).QueryRow(ctx, `SELECT substring(b.bytes FROM 1 FOR $3) FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind='chunk' AND rb.hash=$2 FOR SHARE OF rb,b`, repo, chunk, maxStoredRangeChunkBytes+1).Scan(&body)
		if err == nil && len(body) > maxStoredRangeChunkBytes {
			return nil, domain.ErrIntegrity
		}
		return body, mapNoRows(err)
	}, spool)
	if err != nil {
		return err
	}
	return s.putRangeIndex(ctx, repo, index)
}

func (s *PostgresStore) EnsureDocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) error {
	if _, err := s.DocRangeIndex(ctx, repo, hash); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		return err
	}
	if outbound.DocReadOnly(ctx) {
		return domain.ErrDocRangeIndexUnavailable
	}
	if bound, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx); ok {
		if bound.owner != s || bound.readOnly || bound.repo != repo {
			return domain.ErrConflict
		}
		return s.ensureRangeIndex(ctx, repo, hash)
	}
	for attempt := 0; attempt < 3; attempt++ {
		err := s.buildRangeIndexTransaction(ctx, repo, hash)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "40001" || attempt == 2 {
			return err
		}
	}
	return domain.ErrConflict
}

func (s *PostgresStore) buildRangeIndexTransaction(ctx context.Context, repo, hash domain.ContentHash) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: tx, owner: s, repo: repo})
	if err := s.ensureRangeIndex(bound, repo, hash); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var _ outbound.DocRangeStore = (*PostgresStore)(nil)
