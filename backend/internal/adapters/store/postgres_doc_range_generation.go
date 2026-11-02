//go:build postgres

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const docRangeGenerationCacheObjects = 16

const docRangeEnvelopeSourceSQL = `
 FROM repo_blobs owned JOIN blobs source ON source.hash=owned.hash
 JOIN doc_range_generations_v2 generation ON generation.doc_hash=source.hash
 JOIN doc_legacy_byte_stages_v1 stage ON stage.doc_hash=source.hash AND stage.plan_hash=generation.legacy_byte_plan_hash
 JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash`

const docRangeEnvelopeFenceSQL = `
 WHERE owned.repo_id=$1 AND owned.kind='doc' AND owned.hash=$2
 AND generation.generation_id::text=$3 AND generation.generation_hash=$4
 AND generation.source_version=$5 AND generation.source_version=source.doc_range_source_version
 AND generation.range_layout_version=6 AND stage.source_id=incarnation.incarnation
 AND stage.source_version=generation.source_version AND stage.complete AND stage.next_block=stage.blocks
 AND stage.header_version=1 AND stage.header_complete AND stage.header_bytes=$6
 AND stage.header_next=(stage.header_bytes+65535)/65536`

type postgresDocRangeGenerationRead struct {
	transaction *repositoryTx
	repo        domain.ContentHash
	generation  domain.DocRangeGeneration
	binding     domain.DocReadGenerationBinding
	objects     map[domain.ContentHash][]byte
	order       [docRangeGenerationCacheObjects]domain.ContentHash
	next        int
}

func (s *PostgresStore) readRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (domain.DocRangeGeneration, error) {
	generation, _, err := s.readRangeGenerationBinding(ctx, repo, hash)
	return generation, err
}

func (s *PostgresStore) readRangeGenerationBinding(ctx context.Context, repo, hash domain.ContentHash) (domain.DocRangeGeneration, domain.DocReadGenerationBinding, error) {
	if err := ctx.Err(); err != nil {
		return domain.DocRangeGeneration{}, domain.DocReadGenerationBinding{}, err
	}
	if err := validateHashes(repo, hash); err != nil {
		return domain.DocRangeGeneration{}, domain.DocReadGenerationBinding{}, err
	}
	var binding domain.DocReadGenerationBinding
	var sourceID *string
	var descriptorHash *domain.ContentHash
	var raw []byte
	err := s.db(ctx).QueryRow(ctx, `SELECT generation.generation_id::text,root.doc_range_source_version,
 substring(generation.generation_hash FROM 1 FOR $4),substring(generation.descriptor FROM 1 FOR $3)
 FROM repo_blobs owned JOIN blobs root ON root.hash=owned.hash
 LEFT JOIN doc_range_generations_v2 generation ON generation.doc_hash=root.hash AND generation.source_version=root.doc_range_source_version
 WHERE owned.repo_id=$1 AND owned.kind='doc' AND owned.hash=$2`, repo, hash, domain.MaxDocRangeGenerationBytes+1, len(hash)+1).Scan(&sourceID, &binding.SourceVersion, &descriptorHash, &raw)
	if err != nil {
		return domain.DocRangeGeneration{}, domain.DocReadGenerationBinding{}, mapNoRows(err)
	}
	if descriptorHash == nil {
		return domain.DocRangeGeneration{}, domain.DocReadGenerationBinding{}, domain.ErrDocRangeIndexUnavailable
	}
	if sourceID == nil {
		return domain.DocRangeGeneration{}, domain.DocReadGenerationBinding{}, domain.ErrIntegrity
	}
	generation, err := domain.DecodeDocRangeGeneration(hash, *descriptorHash, raw)
	if err != nil {
		return domain.DocRangeGeneration{}, domain.DocReadGenerationBinding{}, err
	}
	binding.SourceID, binding.RangeGeneration = *sourceID, *descriptorHash
	if err := binding.Validate(); err != nil {
		return domain.DocRangeGeneration{}, domain.DocReadGenerationBinding{}, err
	}
	return generation, binding, ctx.Err()
}

func (s *PostgresStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	reader, err := s.openDocRangeGeneration(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	return reader, nil
}

func (s *PostgresStore) openDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (*postgresDocRangeGenerationRead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateHashes(repo, hash); err != nil {
		return nil, err
	}
	if !s.InReadOnlyTransaction(ctx) {
		return nil, domain.ErrConflict
	}
	bound := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if prior := bound.docRange; bound.readLive != nil && prior != nil && prior.transaction == bound && prior.repo == repo && prior.generation.Hash == hash {
		if err := prior.checkTransaction(ctx); err == nil {
			return prior, nil
		}
	}
	var pinned bool
	if err := s.db(ctx).QueryRow(ctx, `SELECT current_setting('transaction_isolation')='repeatable read'
 AND current_setting('transaction_read_only')='on'`).Scan(&pinned); err != nil {
		return nil, err
	}
	if !pinned {
		return nil, domain.ErrConflict
	}
	generation, binding, err := s.readRangeGenerationBinding(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	reader := &postgresDocRangeGenerationRead{
		transaction: bound,
		repo:        repo,
		generation:  generation,
		binding:     binding,
		objects:     make(map[domain.ContentHash][]byte),
	}
	if bound.readLive != nil {
		bound.docRange = reader
	}
	return reader, nil
}

func (read *postgresDocRangeGenerationRead) Descriptor() domain.DocRangeGeneration {
	generation := read.generation
	generation.Envelope = bytes.Clone(generation.Envelope)
	if generation.EnvelopeRef != nil {
		reference := *generation.EnvelopeRef
		generation.EnvelopeRef = &reference
	}
	if generation.SourceIdentity != nil {
		identity := *generation.SourceIdentity
		generation.SourceIdentity = &identity
	}
	return generation
}

func (read *postgresDocRangeGenerationRead) Envelope(ctx context.Context) (json.RawMessage, error) {
	if err := read.checkTransaction(ctx); err != nil {
		return nil, err
	}
	if read.generation.Version != 6 {
		envelope := bytes.Clone(read.generation.Envelope)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return envelope, nil
	}
	reference := read.generation.EnvelopeRef
	if reference == nil || len(read.generation.Envelope) != 0 || domain.ValidateContentHash(reference.Hash) != nil ||
		reference.Bytes <= domain.MaxConversationManifestBytes || reference.Bytes > domain.MaxConversationDocumentBytes {
		return nil, domain.ErrIntegrity
	}
	if reference.Bytes > maxLegacyReadBytes {
		return nil, domain.ErrDocumentResourceLimit
	}
	var stageID string
	var plan domain.ContentHash
	err := read.transaction.QueryRow(ctx, `SELECT stage.stage_id::text,substring(stage.plan_hash FROM 1 FOR 72)`+docRangeEnvelopeSourceSQL+docRangeEnvelopeFenceSQL+`
 AND (SELECT count(*) FROM doc_legacy_byte_stage_headers_v1 part WHERE part.doc_hash=stage.doc_hash)=stage.header_next`,
		read.repo, read.generation.Hash, read.binding.SourceID, read.binding.RangeGeneration, read.binding.SourceVersion, reference.Bytes).
		Scan(&stageID, &plan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrIntegrity
	}
	if err != nil {
		return nil, err
	}
	if stageID == "" || domain.ValidateContentHash(plan) != nil {
		return nil, domain.ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	envelope := make(json.RawMessage, reference.Bytes)
	hasher := sha256.New()
	for offset, ordinal := 0, 0; offset < len(envelope); ordinal++ {
		if err := read.checkTransaction(ctx); err != nil {
			return nil, err
		}
		length := min(legacyByteHeaderPartBytes, len(envelope)-offset)
		var storedOrdinal int
		var hash domain.ContentHash
		var body []byte
		err := read.transaction.QueryRow(ctx, `SELECT part.ordinal,substring(part.hash FROM 1 FOR 72),
 CASE WHEN octet_length(part.bytes)=$10 THEN substring(part.bytes FROM 1 FOR $10) END`+
			docRangeEnvelopeSourceSQL+`
 JOIN doc_legacy_byte_stage_headers_v1 part ON part.doc_hash=stage.doc_hash`+docRangeEnvelopeFenceSQL+`
 AND stage.stage_id::text=$7 AND stage.plan_hash=$8 AND part.ordinal=$9`,
			read.repo, read.generation.Hash, read.binding.SourceID, read.binding.RangeGeneration, read.binding.SourceVersion,
			reference.Bytes, stageID, plan, ordinal, length).Scan(&storedOrdinal, &hash, &body)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrIntegrity
		}
		if err != nil {
			return nil, err
		}
		if storedOrdinal != ordinal || len(body) != length || domain.HashContent(body) != hash {
			return nil, domain.ErrIntegrity
		}
		copy(envelope[offset:], body)
		_, _ = hasher.Write(body)
		offset += length
	}
	if domain.ContentHash("sha256:"+hex.EncodeToString(hasher.Sum(nil))) != reference.Hash {
		return nil, domain.ErrIntegrity
	}
	if err := read.checkTransaction(ctx); err != nil {
		return nil, err
	}
	return envelope, nil
}

func (read *postgresDocRangeGenerationRead) Binding() domain.DocReadGenerationBinding {
	return read.binding
}

func (read *postgresDocRangeGenerationRead) checkTransaction(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bound, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || bound != read.transaction || !bound.readOnly {
		return domain.ErrConflict
	}
	if err := bound.SendBatch(ctx, &pgx.Batch{}).Close(); err != nil {
		return err
	}
	if bound.readLive != nil {
		return bound.readLive()
	}
	_, err := bound.Exec(ctx, `SELECT 1`)
	return err
}

func (read *postgresDocRangeGenerationRead) loadObject(ctx context.Context, hash domain.ContentHash, limit int) ([]byte, error) {
	if err := read.checkTransaction(ctx); err != nil {
		return nil, err
	}
	if raw, ok := read.objects[hash]; ok {
		if len(raw) > limit {
			return nil, domain.ErrIntegrity
		}
		return raw, nil
	}
	var raw []byte
	err := read.transaction.QueryRow(ctx, `SELECT substring(bytes FROM 1 FOR $3)
 FROM doc_range_objects_v2 WHERE doc_hash=$1 AND hash=$2`, read.generation.Hash, hash, limit+1).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrIntegrity
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > limit || domain.HashContent(raw) != hash {
		return nil, domain.ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	delete(read.objects, read.order[read.next])
	read.order[read.next] = hash
	read.next = (read.next + 1) % len(read.order)
	read.objects[hash] = raw
	return raw, nil
}

func (read *postgresDocRangeGenerationRead) loadDirectory(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
	return read.loadObject(ctx, hash, domain.MaxDocRangeDirectoryBytes)
}

func (read *postgresDocRangeGenerationRead) PageHash(ctx context.Context, position int) (domain.ContentHash, error) {
	if err := read.checkTransaction(ctx); err != nil {
		return "", err
	}
	return domain.ResolveDocRangeLeaf(ctx, read.generation.EventPages, position, read.loadDirectory)
}

func (read *postgresDocRangeGenerationRead) ReadPage(ctx context.Context, position int) ([]domain.DocRangeEntry, error) {
	hash, err := read.PageHash(ctx, position)
	if err != nil {
		return nil, err
	}
	raw, err := read.loadObject(ctx, hash, domain.MaxDocRangePageBytes)
	if err != nil {
		return nil, err
	}
	entries, err := read.generation.DecodePage(position, hash, raw)
	if err != nil {
		return nil, err
	}
	return entries, ctx.Err()
}

func (read *postgresDocRangeGenerationRead) Chunk(ctx context.Context, ordinal int) (domain.ConversationManifestChunk, error) {
	if err := read.checkTransaction(ctx); err != nil {
		return domain.ConversationManifestChunk{}, err
	}
	if ordinal < 0 || ordinal >= read.generation.ChunkCount() {
		return domain.ConversationManifestChunk{}, domain.ErrIntegrity
	}
	position := ordinal / domain.DocRangeChunkPageSize
	hash, err := domain.ResolveDocRangeLeaf(ctx, read.generation.ChunkPages, position, read.loadDirectory)
	if err != nil {
		return domain.ConversationManifestChunk{}, err
	}
	raw, err := read.loadDirectory(ctx, hash)
	if err != nil {
		return domain.ConversationManifestChunk{}, err
	}
	chunks, err := read.generation.DecodeChunks(position, hash, raw)
	if err != nil {
		return domain.ConversationManifestChunk{}, err
	}
	return chunks[ordinal%domain.DocRangeChunkPageSize], ctx.Err()
}

var _ outbound.DocRangeGenerationStore = (*PostgresStore)(nil)
