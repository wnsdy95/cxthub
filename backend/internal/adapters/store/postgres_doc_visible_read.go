//go:build postgres

package store

import (
	"context"
	"maps"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type postgresDocVisibleGenerationRead struct {
	gate       chan struct{}
	generation domain.DocVisibleGeneration
	binding    domain.DocReadGenerationBinding
	objects    postgresDocRangeGenerationRead
	reader     *domain.DocVisibleReader
}

func (s *PostgresStore) readVisibleGeneration(ctx context.Context, repo, hash domain.ContentHash) (domain.DocVisibleGeneration, error) {
	generation, _, err := s.readVisibleGenerationBinding(ctx, repo, hash)
	return generation, err
}

func (s *PostgresStore) readVisibleGenerationBinding(ctx context.Context, repo, hash domain.ContentHash) (domain.DocVisibleGeneration, domain.DocReadGenerationBinding, error) {
	if err := ctx.Err(); err != nil {
		return domain.DocVisibleGeneration{}, domain.DocReadGenerationBinding{}, err
	}
	if err := validateHashes(repo, hash); err != nil {
		return domain.DocVisibleGeneration{}, domain.DocReadGenerationBinding{}, err
	}
	var binding domain.DocReadGenerationBinding
	var sourceID *string
	var rangeHash, descriptorHash *domain.ContentHash
	var rangeRaw, raw []byte
	err := s.db(ctx).QueryRow(ctx, `SELECT ranges.generation_id::text,root.doc_range_source_version,
	 substring(ranges.generation_hash FROM 1 FOR $3),substring(ranges.descriptor FROM 1 FOR $4),
 substring(visible.descriptor_hash FROM 1 FOR $3),substring(visible.descriptor FROM 1 FOR $5)
 FROM repo_blobs owned JOIN blobs root ON root.hash=owned.hash
 LEFT JOIN doc_range_generations_v2 ranges ON ranges.doc_hash=root.hash AND ranges.source_version=root.doc_range_source_version
 LEFT JOIN doc_visible_generations_v1 visible ON visible.doc_hash=ranges.doc_hash
 AND visible.source_version=root.doc_range_source_version AND visible.range_generation_hash=ranges.generation_hash
 WHERE owned.repo_id=$1 AND owned.kind='doc' AND owned.hash=$2`, repo, hash, len(hash)+1,
		domain.MaxDocRangeGenerationBytes+1, domain.MaxDocVisibleGenerationBytes+1).Scan(&sourceID, &binding.SourceVersion, &rangeHash, &rangeRaw, &descriptorHash, &raw)
	if err != nil {
		return domain.DocVisibleGeneration{}, domain.DocReadGenerationBinding{}, mapNoRows(err)
	}
	if rangeHash == nil || descriptorHash == nil {
		return domain.DocVisibleGeneration{}, domain.DocReadGenerationBinding{}, domain.ErrDocRangeIndexUnavailable
	}
	if sourceID == nil {
		return domain.DocVisibleGeneration{}, domain.DocReadGenerationBinding{}, domain.ErrIntegrity
	}
	ranges, err := domain.DecodeDocRangeGeneration(hash, *rangeHash, rangeRaw)
	if err != nil {
		return domain.DocVisibleGeneration{}, domain.DocReadGenerationBinding{}, err
	}
	generation, err := domain.DecodeDocVisibleGeneration(hash, *rangeHash, *descriptorHash, raw)
	if err != nil {
		return domain.DocVisibleGeneration{}, domain.DocReadGenerationBinding{}, err
	}
	if int64(generation.Events) != ranges.EventCount {
		return domain.DocVisibleGeneration{}, domain.DocReadGenerationBinding{}, domain.ErrIntegrity
	}
	binding.SourceID, binding.RangeGeneration = *sourceID, *rangeHash
	if err := binding.Validate(); err != nil {
		return domain.DocVisibleGeneration{}, domain.DocReadGenerationBinding{}, err
	}
	return generation, binding, ctx.Err()
}

func (s *PostgresStore) OpenDocVisibleGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocVisibleGenerationRead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !s.InReadOnlyTransaction(ctx) {
		return nil, domain.ErrConflict
	}
	var pinned bool
	if err := s.db(ctx).QueryRow(ctx, `SELECT current_setting('transaction_isolation')='repeatable read'
 AND current_setting('transaction_read_only')='on'`).Scan(&pinned); err != nil {
		return nil, err
	}
	if !pinned {
		return nil, domain.ErrConflict
	}
	generation, binding, err := s.readVisibleGenerationBinding(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	read := &postgresDocVisibleGenerationRead{
		gate:       make(chan struct{}, 1),
		generation: generation,
		binding:    binding,
		objects: postgresDocRangeGenerationRead{
			transaction: ctx.Value(repositoryTxKey{}).(*repositoryTx),
			generation:  domain.DocRangeGeneration{Hash: generation.Hash},
			objects:     make(map[domain.ContentHash][]byte),
		},
	}
	read.reader, err = domain.NewDocVisibleReader(generation, read.loadObject)
	if err != nil {
		return nil, err
	}
	return read, nil
}

func (read *postgresDocVisibleGenerationRead) Descriptor() domain.DocVisibleGeneration {
	generation := read.generation
	generation.Views = maps.Clone(generation.Views)
	return generation
}

func (read *postgresDocVisibleGenerationRead) Binding() domain.DocReadGenerationBinding {
	return read.binding
}

func (read *postgresDocVisibleGenerationRead) loadObject(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
	return read.objects.loadObject(ctx, hash, domain.MaxDocVisibleObjectBytes)
}

func (read *postgresDocVisibleGenerationRead) Unit(ctx context.Context, mode string, position int, byEvent bool) (domain.DocVisibleUnit, int, error) {
	if err := ctx.Err(); err != nil {
		return domain.DocVisibleUnit{}, 0, err
	}
	if read.gate == nil || read.reader == nil {
		return domain.DocVisibleUnit{}, 0, domain.ErrConflict
	}
	select {
	case read.gate <- struct{}{}:
		defer func() { <-read.gate }()
	case <-ctx.Done():
		return domain.DocVisibleUnit{}, 0, ctx.Err()
	}
	if err := read.objects.checkTransaction(ctx); err != nil {
		return domain.DocVisibleUnit{}, 0, err
	}
	return read.reader.Unit(ctx, mode, position, byEvent)
}

var _ outbound.DocVisibleGenerationStore = (*PostgresStore)(nil)
