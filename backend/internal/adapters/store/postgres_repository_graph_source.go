//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.RepositoryGraphSourceGenerationStore = (*PostgresStore)(nil)

const repositoryGraphSourceStateSQL = `SELECT catalog.epoch::text,catalog.head_seq,catalog.floor_seq,
	CASE WHEN graph.epoch=source.epoch THEN graph.epoch::text ELSE '' END,
	GREATEST(graph.baseline_seq,source.baseline_seq)
	FROM repository_catalog_state catalog JOIN repository_view_source_state source USING(repo_id)
	JOIN repository_graph_source_state graph USING(repo_id) WHERE catalog.repo_id=$1`

func (s *PostgresStore) ReadRepositoryGraphSourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.RepositoryViewSourceCheckpoint{}, err
	}
	checkpoint := domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope, RepoID: repo}
	err := s.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if !s.InReadOnlyTransaction(bound) {
			return domain.ErrConflict
		}
		if err := s.checkRepositoryDocumentIdentity(bound, s.db(bound), repo, false); err != nil {
			return err
		}
		var sourceEpoch string
		var floor, baseline int64
		if err := s.db(bound).QueryRow(bound, repositoryGraphSourceStateSQL, string(repo)).Scan(&checkpoint.Epoch, &checkpoint.Sequence, &floor, &sourceEpoch, &baseline); err != nil {
			return mapNoRows(err)
		}
		if checkpoint.Validate() != nil || checkpoint.Epoch != sourceEpoch || floor < 0 || baseline < 0 || floor > checkpoint.Sequence || baseline > checkpoint.Sequence {
			return domain.ErrIntegrity
		}
		return nil
	})
	if err != nil {
		return domain.RepositoryViewSourceCheckpoint{}, err
	}
	return checkpoint, nil
}
