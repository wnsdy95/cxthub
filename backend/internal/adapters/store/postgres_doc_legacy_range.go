//go:build postgres

package store

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (storage *PostgresStore) ConfigureLegacyByteRangePublication(enabled bool) {
	storage.legacyByteRangePublication = enabled
}

func (storage *PostgresStore) ensureLegacyRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) error {
	return storage.ensureLegacyRangeGenerationMinimum(ctx, repo, hash, 0)
}

func (storage *PostgresStore) ensureLegacyRangeGenerationMinimum(ctx context.Context, repo, hash domain.ContentHash, minimum int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bound, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || bound.owner != storage || bound.readOnly || bound.repo != repo || outbound.DocReadOnly(ctx) {
		return domain.ErrConflict
	}
	if err := validateHashes(repo, hash); err != nil {
		return err
	}
	var version int64
	var raw []byte
	if err := bound.QueryRow(ctx, `SELECT source.doc_range_source_version,substring(source.bytes FROM 1 FOR $3)
 FROM repo_blobs owned JOIN blobs source ON source.hash=owned.hash
 WHERE owned.repo_id=$1 AND owned.kind='doc' AND owned.hash=$2 FOR UPDATE OF owned,source`, repo, hash, 2*maxLegacyReadBytes+1).Scan(&version, &raw); err != nil {
		return mapNoRows(err)
	}
	if version < 0 {
		return domain.ErrIntegrity
	}
	body, err := boundedLegacyObject(ctx, raw, maxLegacyReadBytes)
	if err != nil {
		return err
	}
	manifest, chunked := domain.ParseDocChunkManifest(body)
	if chunked {
		if err := lockLegacyRangeChunks(ctx, bound, repo, manifest); err != nil {
			return err
		}
	}
	generation, err := storage.readRangeGeneration(ctx, repo, hash)
	available := err == nil
	if available && generation.Identity() != domain.DocumentIdentityLegacy {
		return domain.ErrIntegrity
	}
	if err != nil && !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		return err
	}
	if chunked && manifest.Format == domain.ChunkFormatV2 {
		spool, err := newLegacyDocSpool()
		if err != nil {
			return err
		}
		defer spool.Close()
		verified, err := storage.docProofs.rootVerifier.VerifyLegacyStream(ctx, hash, manifest, func(ctx context.Context, chunk domain.ContentHash) ([]byte, error) {
			return storage.legacyJobChunk(ctx, repo, chunk, true)
		}, spool)
		if err != nil {
			return err
		}
		if !verified.Valid() || verified.Reference().DocumentRef() != (domain.DocumentRef{Hash: hash, Identity: domain.DocumentIdentityLegacy}) {
			return domain.ErrIntegrity
		}
		if available && (minimum == 0 || generation.Version >= 4 || generation.StreamBytes == 0) {
			return ctx.Err()
		}
		var generation domain.DocRangeGeneration
		var objects []domain.DocRangeObject
		if storage.legacyByteRangePublication {
			generation, objects, err = domain.BuildStreamedLegacyDocByteRangeGeneration(ctx, verified, manifest)
		} else {
			generation, objects, err = domain.BuildStreamedLegacyDocRangeGeneration(ctx, verified)
		}
		if err != nil {
			return err
		}
		if minimum >= 4 && generation.Version < 4 && generation.StreamBytes != 0 {
			return domain.ErrDocByteRangeBackfillRequired
		}
		return storage.putLegacyRangeObjects(ctx, repo, generation, objects)
	}
	verified, err := storage.verifyLegacyRangeFallback(ctx, repo, hash, body, manifest, chunked)
	if err != nil {
		return err
	}
	if !verified.Valid() || verified.DocumentRef() != (domain.DocumentRef{Hash: hash, Identity: domain.DocumentIdentityLegacy}) {
		return domain.ErrIntegrity
	}
	if available && (minimum == 0 || generation.Version >= 4 || generation.StreamBytes == 0) {
		return ctx.Err()
	}
	if minimum >= 4 {
		generation, objects, err := domain.BuildLegacyDocRangeGeneration(ctx, verified)
		if err != nil {
			return err
		}
		if generation.StreamBytes != 0 {
			return domain.ErrDocByteRangeBackfillRequired
		}
		return storage.putLegacyRangeObjects(ctx, repo, generation, objects)
	}
	return storage.putLegacyRangeGeneration(ctx, repo, verified)
}

func (storage *PostgresStore) verifyLegacyRangeFallback(ctx context.Context, repo, hash domain.ContentHash, body []byte, manifest domain.DocChunkManifest, chunked bool) (domain.VerifiedSessionDoc, error) {
	if !chunked {
		verified, err := verifyLegacyDecodedMonolithicDoc(ctx, hash, body)
		if err != nil {
			return domain.VerifiedSessionDoc{}, err
		}
		return verified, ctx.Err()
	}
	if len(manifest.Chunks) > domain.MaxDocJobChunks {
		return domain.VerifiedSessionDoc{}, domain.ErrDocumentResourceLimit
	}
	total := len(manifest.Envelope) + len(manifest.Chunks) + 64
	if total > maxLegacyReadBytes {
		return domain.VerifiedSessionDoc{}, domain.ErrDocumentResourceLimit
	}
	var verifier domain.CanonicalDocVerifier
	return verifier.VerifyChunks(ctx, hash, manifest, func(ctx context.Context, chunk domain.ContentHash) ([]byte, error) {
		body, err := storage.legacyJobChunk(ctx, repo, chunk, true)
		if err != nil {
			return nil, err
		}
		if len(body) > maxLegacyReadBytes-total {
			return nil, domain.ErrDocumentResourceLimit
		}
		total += len(body)
		return body, nil
	})
}

func lockLegacyRangeChunks(ctx context.Context, transaction *repositoryTx, repo domain.ContentHash, manifest domain.DocChunkManifest) error {
	hashes := slices.Clone(manifest.Chunks)
	slices.Sort(hashes)
	hashes = slices.Compact(hashes)
	for start := 0; start < len(hashes); start += 128 {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch := hashes[start:min(start+128, len(hashes))]
		rows, err := transaction.Query(ctx, `SELECT chunk.hash FROM repo_blobs owned JOIN blobs chunk ON chunk.hash=owned.hash
 WHERE owned.repo_id=$1 AND owned.kind='chunk' AND owned.hash=ANY($2::text[])
 ORDER BY chunk.hash FOR SHARE OF owned,chunk`, repo, batch)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			count++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if count != len(batch) {
			return domain.ErrNotFound
		}
	}
	return ctx.Err()
}

func (storage *PostgresStore) putLegacyRangeGeneration(ctx context.Context, repo domain.ContentHash, verified domain.VerifiedSessionDoc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bound, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || bound.owner != storage || bound.readOnly || bound.repo != repo || outbound.DocReadOnly(ctx) {
		return domain.ErrConflict
	}
	if !verified.Valid() || verified.DocumentRef().Identity != domain.DocumentIdentityLegacy {
		return domain.ErrIntegrity
	}
	generation, objects, err := domain.BuildLegacyDocRangeGeneration(ctx, verified)
	if err != nil {
		return err
	}
	if generation.Hash != verified.Hash() {
		return domain.ErrIntegrity
	}
	return storage.putLegacyRangeObjects(ctx, repo, generation, objects)
}

func (storage *PostgresStore) putLegacyRangeObjects(ctx context.Context, repo domain.ContentHash, generation domain.DocRangeGeneration, objects []domain.DocRangeObject) error {
	return storage.putLegacyRangeObjectsForPlan(ctx, repo, generation, objects, "")
}

func (storage *PostgresStore) putLegacyRangeObjectsForPlan(ctx context.Context, repo domain.ContentHash, generation domain.DocRangeGeneration, objects []domain.DocRangeObject, plan domain.ContentHash) error {
	return storage.putLegacyRangeObjectReaderForPlan(ctx, repo, generation, len(objects), func(_ context.Context, ordinal int) (domain.DocRangeObject, error) {
		return objects[ordinal], nil
	}, plan)
}

func (storage *PostgresStore) putLegacyRangeObjectReaderForPlan(ctx context.Context, repo domain.ContentHash, generation domain.DocRangeGeneration, count int, read func(context.Context, int) (domain.DocRangeObject, error), plan domain.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if count < 0 || read == nil {
		return domain.ErrIntegrity
	}
	bound, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || bound.owner != storage || bound.readOnly || bound.repo != repo || outbound.DocReadOnly(ctx) {
		return domain.ErrConflict
	}
	validMode := (generation.Version == 3 || generation.Version == 4) && plan == "" ||
		(generation.Version == 5 || generation.Version == 6) && domain.ValidateContentHash(plan) == nil
	if !validMode || generation.Identity() != domain.DocumentIdentityLegacy {
		return domain.ErrIntegrity
	}
	descriptor, err := generation.Encode()
	if err != nil {
		return err
	}
	tag, err := bound.Exec(ctx, `INSERT INTO doc_range_generations_v2(doc_hash,source_version,generation_hash,descriptor,legacy_byte_plan_hash)
 SELECT source.hash,source.doc_range_source_version,$3,$4,NULLIF($5,'') FROM repo_blobs owned JOIN blobs source ON source.hash=owned.hash
 WHERE owned.repo_id=$1 AND owned.kind='doc' AND owned.hash=$2
 ON CONFLICT(doc_hash) DO UPDATE SET source_version=EXCLUDED.source_version,generation_hash=EXCLUDED.generation_hash,descriptor=EXCLUDED.descriptor,
 legacy_byte_plan_hash=EXCLUDED.legacy_byte_plan_hash`, repo, generation.Hash, domain.HashContent(descriptor), descriptor, plan)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	if _, err := bound.Exec(ctx, `DELETE FROM doc_range_objects_v2 WHERE doc_hash=$1`, generation.Hash); err != nil {
		return err
	}
	for start := 0; start < count; start += 128 {
		batch := &pgx.Batch{}
		for ordinal := start; ordinal < min(start+128, count); ordinal++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			object, err := read(ctx, ordinal)
			if err != nil {
				return err
			}
			if len(object.Bytes) == 0 || len(object.Bytes) > domain.MaxDocRangePageBytes || domain.HashContent(object.Bytes) != object.Hash {
				return domain.ErrIntegrity
			}
			batch.Queue(`INSERT INTO doc_range_objects_v2(doc_hash,hash,bytes) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, generation.Hash, object.Hash, object.Bytes)
		}
		if err := bound.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
	}
	return ctx.Err()
}
