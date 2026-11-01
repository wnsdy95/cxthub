//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) readDocDisplayGeneration(ctx context.Context, target domain.DocDisplayTarget) (domain.DocDisplayStringGeneration, error) {
	if target.DisplayProfile() != domain.DocDisplayStringProfile {
		return domain.DocDisplayStringGeneration{}, domain.ErrValidation
	}
	if err := s.requireDocDisplayRead(ctx); err != nil {
		return domain.DocDisplayStringGeneration{}, err
	}
	if _, err := s.proveDocDisplayTarget(ctx, target, false); err != nil {
		return domain.DocDisplayStringGeneration{}, err
	}
	var hash, sourceHash domain.ContentHash
	var raw []byte
	if err := s.db(ctx).QueryRow(ctx, `SELECT substring(generation_hash FROM 1 FOR 72),substring(source_hash FROM 1 FOR 72),
 substring(bytes FROM 1 FOR $3) FROM doc_display_generations_v1 WHERE event_hash=$1 AND profile=$2`,
		target.Event.Hash, domain.DocDisplayStringProfile, domain.MaxDocDisplaySourceBytes+1).Scan(&hash, &sourceHash, &raw); err != nil {
		return domain.DocDisplayStringGeneration{}, mapNoRows(err)
	}
	generation, err := domain.DecodeDocDisplayStringGeneration(hash, raw)
	if err != nil {
		return domain.DocDisplayStringGeneration{}, err
	}
	source, err := s.readDocDisplaySource(ctx, target, false)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.DocDisplayStringGeneration{}, domain.ErrIntegrity
		}
		return domain.DocDisplayStringGeneration{}, err
	}
	hash, err = source.Hash()
	if err != nil || generation.Source != *source || sourceHash != hash {
		return domain.DocDisplayStringGeneration{}, domain.ErrIntegrity
	}
	return generation, ctx.Err()
}

func (s *PostgresStore) ReadDocDisplayGeneration(ctx context.Context, target domain.DocDisplayTarget) (domain.DocDisplayStringGeneration, error) {
	return s.readDocDisplayGeneration(ctx, target)
}

func (s *PostgresStore) ReadDocDisplayPage(ctx context.Context, target domain.DocDisplayTarget, index int) (domain.DocDisplayPage, error) {
	if index < 0 || index >= domain.MaxDocRangePages {
		return domain.DocDisplayPage{}, domain.ErrValidation
	}
	generation, err := s.readDocDisplayGeneration(ctx, target)
	if err != nil {
		return domain.DocDisplayPage{}, err
	}
	if index >= generation.Pages.Count {
		return domain.DocDisplayPage{}, domain.ErrNotFound
	}
	sourceHash, err := generation.Source.Hash()
	if err != nil {
		return domain.DocDisplayPage{}, err
	}
	return domain.ReadDocDisplayPage(ctx, generation, index, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		return s.loadDocDisplayObject(ctx, sourceHash, hash)
	})
}

func (s *PostgresStore) loadDocDisplayObject(ctx context.Context, sourceHash, hash domain.ContentHash) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var raw []byte
	if err := s.db(ctx).QueryRow(ctx, `SELECT substring(bytes FROM 1 FOR $3) FROM doc_display_objects_v1
 WHERE source_hash=$1 AND hash=$2`, sourceHash, hash, domain.MaxDocDisplayPageBytes+1).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrIntegrity
		}
		return nil, err
	}
	if len(raw) == 0 || len(raw) > domain.MaxDocDisplayPageBytes || domain.HashContent(raw) != hash {
		return nil, domain.ErrIntegrity
	}
	return raw, ctx.Err()
}
