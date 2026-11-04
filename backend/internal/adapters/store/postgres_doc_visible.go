//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const docVisibleBatchObjects = 128
const docVisibleBatchBytes = 4 << 20

type postgresDocVisibleBatch struct {
	transaction *repositoryTx
	hash        domain.ContentHash
	batch       pgx.Batch
	bytes       int
}

func (batch *postgresDocVisibleBatch) flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if batch.batch.Len() == 0 {
		return nil
	}
	if err := batch.transaction.SendBatch(ctx, &batch.batch).Close(); err != nil {
		return err
	}
	batch.batch = pgx.Batch{}
	batch.bytes = 0
	return ctx.Err()
}

func (batch *postgresDocVisibleBatch) emit(ctx context.Context, object domain.DocRangeObject) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(object.Bytes) == 0 || len(object.Bytes) > domain.MaxDocVisibleObjectBytes || domain.HashContent(object.Bytes) != object.Hash {
		return domain.ErrIntegrity
	}
	if err := batch.reserve(ctx, 1, len(object.Bytes)); err != nil {
		return err
	}
	batch.batch.Queue(`INSERT INTO doc_range_objects_v2(doc_hash,hash,bytes) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, batch.hash, object.Hash, object.Bytes)
	batch.bytes += len(object.Bytes)
	return nil
}

func (batch *postgresDocVisibleBatch) reserve(ctx context.Context, statements, payloadBytes int) error {
	if batch.batch.Len()+statements > docVisibleBatchObjects || batch.bytes+payloadBytes > docVisibleBatchBytes {
		if err := batch.flush(ctx); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (batch *postgresDocVisibleBatch) emitRangePage(ctx context.Context, position int, object domain.DocRangeObject) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if position < 0 || len(object.Bytes) == 0 || len(object.Bytes) > domain.MaxDocRangePageBytes || domain.HashContent(object.Bytes) != object.Hash {
		return domain.ErrIntegrity
	}
	if err := batch.reserve(ctx, 2, 2*len(object.Bytes)); err != nil {
		return err
	}
	batch.batch.Queue(`INSERT INTO doc_range_pages_v1(doc_hash,ordinal,bytes) VALUES($1,$2,$3)`, batch.hash, position, object.Bytes)
	batch.batch.Queue(`INSERT INTO doc_range_objects_v2(doc_hash,hash,bytes) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, batch.hash, object.Hash, object.Bytes)
	batch.bytes += 2 * len(object.Bytes)
	return nil
}

func (s *PostgresStore) putVisibleGeneration(ctx context.Context, repo domain.ContentHash, proof domain.VerifiedDocRangeGeneration) error {
	return s.putDocVisibleGeneration(ctx, repo, proof, false)
}

func (s *PostgresStore) putDocVisibleGeneration(ctx context.Context, repo domain.ContentHash, proof domain.VerifiedDocRangeGeneration, includeRangePages bool) error {
	bound, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || bound.owner != s || bound.readOnly || bound.repo != repo {
		return domain.ErrConflict
	}
	batch := postgresDocVisibleBatch{transaction: bound, hash: proof.Descriptor().Hash}
	var generation domain.DocVisibleGeneration
	var err error
	if includeRangePages {
		generation, err = domain.BuildDocVisibleGenerationWithRangePages(ctx, proof, batch.emit, batch.emitRangePage)
	} else {
		generation, err = domain.BuildDocVisibleGeneration(ctx, proof, batch.emit)
	}
	if err != nil {
		return err
	}
	if err := batch.flush(ctx); err != nil {
		return err
	}
	descriptor, err := generation.Encode()
	if err != nil {
		return err
	}
	return writeVisibleGenerationPG(ctx, bound, generation.Hash, generation.RangeGeneration, domain.HashContent(descriptor), descriptor)
}

func writeVisibleGenerationPG(ctx context.Context, transaction *repositoryTx, hash, ranges, visible domain.ContentHash, descriptor []byte) error {
	tag, err := transaction.Exec(ctx, `INSERT INTO doc_visible_generations_v1(doc_hash,source_version,range_generation_hash,descriptor_hash,descriptor)
 SELECT root.hash,root.doc_range_source_version,ranges.generation_hash,$3,$4
 FROM blobs root JOIN doc_range_generations_v2 ranges ON ranges.doc_hash=root.hash
 WHERE root.hash=$1 AND ranges.generation_hash=$2 AND ranges.source_version=root.doc_range_source_version
 ON CONFLICT(doc_hash) DO UPDATE SET source_version=EXCLUDED.source_version,range_generation_hash=EXCLUDED.range_generation_hash,
 descriptor_hash=EXCLUDED.descriptor_hash,descriptor=EXCLUDED.descriptor`, hash, ranges, visible, descriptor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrDocRangeIndexUnavailable
	}
	return ctx.Err()
}
