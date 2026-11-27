//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

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
