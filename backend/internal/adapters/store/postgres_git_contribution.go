//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitContributionStore = (*PostgresStore)(nil)

func readGitContributionPG(ctx context.Context, database catalogMerkleReader, repo domain.ContentHash, id string) (domain.GitContribution, error) {
	var raw []byte
	if err := database.QueryRow(ctx, `SELECT payload FROM git_contributions WHERE repo_id=$1 AND id=$2`, repo, id).Scan(&raw); err != nil {
		return domain.GitContribution{}, mapNoRows(err)
	}
	contribution, err := decodePRHistory[domain.GitContribution](raw)
	if err != nil {
		return domain.GitContribution{}, err
	}
	if contribution.RepoID != repo || contribution.ID != id {
		return domain.GitContribution{}, domain.ErrIntegrity
	}
	return contribution, nil
}

func (s *PostgresStore) PutGitContribution(ctx context.Context, contribution domain.GitContribution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateGitContributionWrite(contribution); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	if err := s.checkRepositoryDocumentIdentity(ctx, tx, contribution.RepoID, true); err != nil {
		return err
	}
	raw, err := json.Marshal(contribution)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO git_contributions(repo_id,id,payload) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, contribution.RepoID, contribution.ID, raw); err != nil {
		return err
	}
	old, err := readGitContributionPG(ctx, tx, contribution.RepoID, contribution.ID)
	if err != nil {
		return err
	}
	if old != contribution {
		return domain.ErrConflict
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) ListGitContributions(ctx context.Context, repo domain.ContentHash) ([]domain.GitContribution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT id,payload FROM git_contributions WHERE repo_id=$1 ORDER BY id`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.GitContribution{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		contribution, err := decodePRHistory[domain.GitContribution](raw)
		if err != nil {
			return nil, err
		}
		if contribution.RepoID != repo || contribution.ID != id {
			return nil, domain.ErrIntegrity
		}
		result = append(result, contribution)
	}
	return result, rows.Err()
}

func (s *PostgresStore) PutGitContributionMemory(ctx context.Context, memory domain.GitContributionMemory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateGitContributionMemoryWrite(memory); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	if err := s.checkRepositoryDocumentIdentity(ctx, tx, memory.RepoID, true); err != nil {
		return err
	}
	if _, err := readGitContributionPG(ctx, tx, memory.RepoID, memory.ContributionID); err != nil {
		return err
	}
	raw, err := json.Marshal(memory)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO git_contribution_memories(repo_id,id,contribution_id,publication_id,memory_hash,memory_source,memory_pinned,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, memory.RepoID, memory.ID, memory.ContributionID, memory.PublicationID, memory.MemoryHash, memory.MemorySource, memory.MemoryPinned, raw); err != nil {
		return err
	}
	var stored []byte
	var contributionID, publicationID string
	var memoryHash, memorySource domain.ContentHash
	var memoryPinned bool
	if err := tx.QueryRow(ctx, `SELECT contribution_id,publication_id,memory_hash,memory_source,memory_pinned,payload FROM git_contribution_memories WHERE repo_id=$1 AND id=$2`, memory.RepoID, memory.ID).Scan(&contributionID, &publicationID, &memoryHash, &memorySource, &memoryPinned, &stored); err != nil {
		return mapNoRows(err)
	}
	old, err := decodePRHistory[domain.GitContributionMemory](stored)
	if err != nil {
		return err
	}
	if old.ContributionID != contributionID || old.PublicationID != publicationID || old.MemoryHash != memoryHash || old.MemorySource != memorySource || old.MemoryPinned != memoryPinned || old.RepoID != memory.RepoID || old.ID != memory.ID {
		return domain.ErrIntegrity
	}
	if old != memory {
		return domain.ErrConflict
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) ListGitContributionMemories(ctx context.Context, repo domain.ContentHash) ([]domain.GitContributionMemory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT memory.id,memory.contribution_id,memory.publication_id,memory.memory_hash,memory.memory_source,memory.memory_pinned,memory.payload,contribution.payload
 FROM git_contribution_memories memory LEFT JOIN git_contributions contribution ON contribution.repo_id=memory.repo_id AND contribution.id=memory.contribution_id
 WHERE memory.repo_id=$1 ORDER BY memory.id`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.GitContributionMemory{}
	for rows.Next() {
		var id, contributionID, publicationID string
		var memoryHash, memorySource domain.ContentHash
		var memoryPinned bool
		var raw, contributionRaw []byte
		if err := rows.Scan(&id, &contributionID, &publicationID, &memoryHash, &memorySource, &memoryPinned, &raw, &contributionRaw); err != nil {
			return nil, err
		}
		memory, err := decodePRHistory[domain.GitContributionMemory](raw)
		if err != nil {
			return nil, err
		}
		if memory.RepoID != repo || memory.ID != id || memory.ContributionID != contributionID || memory.PublicationID != publicationID || memory.MemoryHash != memoryHash || memory.MemorySource != memorySource || memory.MemoryPinned != memoryPinned {
			return nil, domain.ErrIntegrity
		}
		contribution, err := decodePRHistory[domain.GitContribution](contributionRaw)
		if err != nil || contribution.RepoID != repo || contribution.ID != memory.ContributionID {
			return nil, errors.Join(domain.ErrIntegrity, err)
		}
		result = append(result, memory)
	}
	return result, rows.Err()
}
