//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const graphQualificationInputSizesSQL = `
	SELECT octet_length(to_jsonb(source)::text) FROM repos source WHERE source.id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM snapshots source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM refs source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM context_history source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM pending_contexts source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM unsync_contexts source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM session_archives source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM reflog source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM repository_revisions source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM git_commit_trees source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM git_tree_nodes source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM git_commit_deltas source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM git_ref_observations source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM pr_history_checkpoints source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM pr_participations source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM pr_contributions source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM git_contributions source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM git_head_receipts source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM git_contribution_admissions source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM git_context_heads source WHERE source.repo_id=$1
	UNION ALL SELECT octet_length(to_jsonb(source)::text) FROM git_contribution_proofs source WHERE source.repo_id=$1
	LIMIT $2`

func (s *PostgresStore) CheckGraphQualificationInputs(ctx context.Context, repo domain.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.InReadOnlyTransaction(ctx) {
		return domain.ErrConflict
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	if err := s.checkRepositoryDocumentIdentity(ctx, s.db(ctx), repo, false); err != nil {
		return err
	}
	rows, err := s.db(ctx).Query(ctx, graphQualificationInputSizesSQL, string(repo), domain.MaxPendingProjectionInputRows+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	var count, total int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var size int64
		if err := rows.Scan(&size); err != nil {
			return err
		}
		if size <= 0 {
			return domain.ErrIntegrity
		}
		count++
		total += size
		if count > domain.MaxPendingProjectionInputRows || size > domain.MaxRepositoryViewSourceImageBytes || total > domain.MaxPendingProjectionInputBytes {
			return domain.ErrRepositoryViewSourceLimit
		}
	}
	return rows.Err()
}
