//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.RepositoryMemorySourceGenerationStore = (*PostgresStore)(nil)

func (storage *PostgresStore) ReadRepositoryMemorySourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryMemorySourceGeneration, error) {
	if domain.ValidateContentHash(repo) != nil {
		return domain.RepositoryMemorySourceGeneration{}, domain.ErrValidation
	}
	generation := domain.RepositoryMemorySourceGeneration{Version: domain.RepositoryMemorySourceVersion}
	err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if !storage.InReadOnlyTransaction(bound) {
			return domain.ErrConflict
		}
		var err error
		generation.Graph, err = storage.ReadRepositoryGraphSourceGeneration(bound, repo)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				if _, lookupErr := storage.GetRepo(bound, repo); lookupErr == nil {
					return domain.ErrIntegrity
				} else if !errors.Is(lookupErr, domain.ErrNotFound) {
					return lookupErr
				}
			}
			return err
		}
		err = storage.db(bound).QueryRow(bound, `SELECT epoch::text,head_seq FROM repository_memory_source_state WHERE repo_id=$1`, string(repo)).
			Scan(&generation.Epoch, &generation.Sequence)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrIntegrity
		}
		if err != nil {
			return err
		}
		if generation.Validate() != nil {
			return domain.ErrIntegrity
		}
		return nil
	})
	if err != nil {
		return domain.RepositoryMemorySourceGeneration{}, err
	}
	return generation, nil
}
