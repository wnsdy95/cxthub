//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (storage *PostgresStore) requireDocDisplayRead(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !storage.InReadOnlyTransaction(ctx) {
		return domain.ErrConflict
	}
	var pinned bool
	if err := storage.db(ctx).QueryRow(ctx, `SELECT current_setting('transaction_isolation')='repeatable read'
 AND current_setting('transaction_read_only')='on'`).Scan(&pinned); err != nil {
		return err
	}
	if !pinned {
		return domain.ErrConflict
	}
	return nil
}

func (storage *PostgresStore) readDocEventDisplayGeneration(ctx context.Context, target domain.DocDisplayTarget) (domain.DocDisplayProjectionGeneration, domain.ContentHash, error) {
	if target.DisplayProfile() != domain.DocDisplayProjectionProfile {
		return domain.DocDisplayProjectionGeneration{}, "", domain.ErrValidation
	}
	if err := storage.requireDocDisplayRead(ctx); err != nil {
		return domain.DocDisplayProjectionGeneration{}, "", err
	}
	entry, err := storage.proveDocDisplayEntry(ctx, target, false)
	if err != nil {
		return domain.DocDisplayProjectionGeneration{}, "", err
	}
	expected, err := domain.NewDocEventDisplayPreparation(entry)
	if err != nil {
		return domain.DocDisplayProjectionGeneration{}, "", err
	}
	var generationHash, sourceHash domain.ContentHash
	var raw, sourceRaw []byte
	if err := storage.db(ctx).QueryRow(ctx, `SELECT substring(generation.generation_hash FROM 1 FOR 72),substring(generation.source_hash FROM 1 FOR 72),
 substring(generation.bytes FROM 1 FOR $3),substring(source.bytes FROM 1 FOR $3)
 FROM doc_display_generations_v1 generation JOIN doc_display_sources_v1 source
 ON source.event_hash=generation.event_hash AND source.profile=generation.profile AND source.source_hash=generation.source_hash
 WHERE generation.event_hash=$1 AND generation.profile=$2`, target.Event.Hash, target.DisplayProfile(),
		domain.MaxDocDisplaySourceBytes+1).Scan(&generationHash, &sourceHash, &raw, &sourceRaw); err != nil {
		return domain.DocDisplayProjectionGeneration{}, "", mapNoRows(err)
	}
	if !equalDocEventDisplaySource(expected.Source, sourceRaw, sourceHash) {
		return domain.DocDisplayProjectionGeneration{}, "", domain.ErrIntegrity
	}
	generation, err := domain.DecodeDocDisplayProjectionGeneration(generationHash, raw)
	if err != nil || generation.Source.EventHash != target.Event.Hash || generation.Source.EventBytes != target.Event.Length || expected.Source.EmptyReasoning && generation.Source.TextBytes != 0 {
		return domain.DocDisplayProjectionGeneration{}, "", domain.ErrIntegrity
	}
	return generation, sourceHash, ctx.Err()
}

func (storage *PostgresStore) ReadDocEventDisplayGeneration(ctx context.Context, target domain.DocDisplayTarget) (domain.DocDisplayProjectionGeneration, error) {
	generation, _, err := storage.readDocEventDisplayGeneration(ctx, target)
	return generation, err
}

func (storage *PostgresStore) ReadDocEventDisplayPage(ctx context.Context, target domain.DocDisplayTarget, index int) (domain.DocDisplayPage, error) {
	if index < 0 || index >= domain.MaxDocRangePages {
		return domain.DocDisplayPage{}, domain.ErrValidation
	}
	generation, sourceHash, err := storage.readDocEventDisplayGeneration(ctx, target)
	if err != nil {
		return domain.DocDisplayPage{}, err
	}
	if index >= generation.Pages.Count {
		return domain.DocDisplayPage{}, domain.ErrNotFound
	}
	return domain.ReadDocDisplayProjectionPage(ctx, generation, index, func(read context.Context, hash domain.ContentHash) ([]byte, error) {
		return storage.loadDocDisplayObject(read, sourceHash, hash)
	})
}
