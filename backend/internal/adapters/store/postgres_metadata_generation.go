//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.RepositoryMetadataGenerationStore = (*PostgresStore)(nil)

func (s *PostgresStore) ReadRepositoryMetadataGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryMetadataGeneration, error) {
	if err := ctx.Err(); err != nil {
		return domain.RepositoryMetadataGeneration{}, err
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.RepositoryMetadataGeneration{}, domain.ErrValidation
	}
	var generation domain.RepositoryMetadataGeneration
	err := s.WithinReadSnapshot(ctx, func(bound context.Context) error {
		transaction, owned := bound.Value(repositoryTxKey{}).(*repositoryTx)
		if !owned || transaction.owner != s || (!transaction.readOnly && transaction.repo != repo) {
			return domain.ErrConflict
		}
		if err := s.checkRepositoryDocumentIdentity(bound, transaction, repo, !transaction.readOnly); err != nil {
			return err
		}
		query := `SELECT repo_id,epoch::text,head_seq FROM repository_catalog_state WHERE repo_id=$1`
		if !transaction.readOnly {
			query += ` FOR UPDATE`
		}
		if err := transaction.QueryRow(bound, query, repo).Scan(&generation.RepoID, &generation.Epoch, &generation.Sequence); err != nil {
			return mapNoRows(err)
		}
		if generation.RepoID != repo || generation.Validate() != nil {
			return domain.ErrIntegrity
		}
		return nil
	})
	if err != nil {
		return domain.RepositoryMetadataGeneration{}, err
	}
	return generation, nil
}
