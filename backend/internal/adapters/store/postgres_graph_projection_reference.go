//go:build postgres

package store

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GraphProjectionReferenceStore = (*PostgresStore)(nil)

var graphProjectionClaimHeldSQL = `work.phase='idle' AND work.source_epoch=catalog.epoch
	AND work.retention_until>clock_timestamp() AND work.pin_seq>=catalog.floor_seq AND work.applied_seq<=catalog.head_seq
	AND EXISTS(SELECT 1 FROM graph_projection_ready ready
		JOIN graph_projection_references reference USING(repo_id)
		JOIN graph_qualification qualification USING(repo_id)
		JOIN repository_view_source_state view_source USING(repo_id)
		JOIN repository_graph_source_state graph_source USING(repo_id)
		WHERE ready.repo_id=work.repo_id AND ready.dataset=work.dataset AND ready.source_epoch=work.source_epoch
		AND ready.source_seq=work.applied_seq AND ready.rules=$1 AND qualification.enabled
		AND view_source.epoch=catalog.epoch AND graph_source.epoch=catalog.epoch
		AND work.applied_seq>=GREATEST(view_source.baseline_seq,graph_source.baseline_seq)
		AND (reference.dataset,reference.source_epoch,reference.source_seq,reference.rules)
			= (ready.dataset,ready.source_epoch,ready.source_seq,ready.rules)
		AND reference.comparison_rules=` + strconv.Itoa(domain.GraphQualificationRules) + `
		AND NOT EXISTS(SELECT 1 FROM graph_serving_repairs repair WHERE repair.repo_id=work.repo_id AND repair.quarantined)
		AND (COALESCE(qualification.lease_until>clock_timestamp()
			AND qualification.attempt_work_dataset=work.dataset
			AND (qualification.attempt_dataset,qualification.attempt_epoch,qualification.attempt_seq,qualification.attempt_rules,qualification.attempt_comparison_rules)
				IS NOT DISTINCT FROM (reference.dataset,reference.source_epoch,reference.source_seq,reference.rules,reference.comparison_rules),false)
		OR (NOT ((qualification.result_dataset,qualification.result_epoch,qualification.result_seq,qualification.result_rules,qualification.result_comparison_rules)
			IS NOT DISTINCT FROM (reference.dataset,reference.source_epoch,reference.source_seq,reference.rules,reference.comparison_rules)
			AND (qualification.result_state='mismatch' OR (qualification.result_state='matched'
				AND EXISTS(SELECT 1 FROM graph_serving_heads head WHERE head.repo_id=work.repo_id
					AND (head.dataset,head.source_epoch,head.source_seq,head.rules,head.comparison_rules)
						= (reference.dataset,reference.source_epoch,reference.source_seq,reference.rules,reference.comparison_rules)
					AND head.expected_hash=qualification.expected_hash AND head.actual_hash=qualification.actual_hash))))
			AND NOT (qualification.reason='resource_limit'
				AND (qualification.attempt_dataset,qualification.attempt_epoch,qualification.attempt_seq,qualification.attempt_rules,qualification.attempt_comparison_rules)
					IS NOT DISTINCT FROM (reference.dataset,reference.source_epoch,reference.source_seq,reference.rules,reference.comparison_rules)))))`

func (s *PostgresStore) SaveGraphProjectionPageWithReference(ctx context.Context, lease domain.GraphProjectionLease, page domain.RepositoryViewSourcePage, reference domain.GraphProjectionReference) error {
	if err := s.graphQualificationWriteContext(ctx); err != nil {
		return err
	}
	checkpoint := domain.RepositoryViewSourceCheckpoint{Version: page.Version, Scope: page.Scope, RepoID: page.RepoID, Epoch: page.Epoch, Sequence: page.Through}
	if reference.Validate() != nil || lease.Cursor != "" || reference.RepoID != lease.RepoID || reference.Dataset != lease.Dataset ||
		reference.Applied != checkpoint || reference.Applied.Epoch != lease.Epoch {
		return domain.ErrValidation
	}
	return s.graphProjection().savePageWith(ctx, domain.PendingProjectionLease(lease), page, func(transaction pgx.Tx) error {
		command, err := transaction.Exec(ctx, `INSERT INTO graph_projection_references
			(repo_id,dataset,source_epoch,source_seq,rules,comparison_rules,expected_hash) VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT(repo_id) DO UPDATE SET dataset=EXCLUDED.dataset,source_epoch=EXCLUDED.source_epoch,
			source_seq=EXCLUDED.source_seq,rules=EXCLUDED.rules,comparison_rules=EXCLUDED.comparison_rules,expected_hash=EXCLUDED.expected_hash
			WHERE (graph_projection_references.dataset,graph_projection_references.source_epoch,graph_projection_references.source_seq,
				graph_projection_references.rules,graph_projection_references.comparison_rules)
				IS DISTINCT FROM (EXCLUDED.dataset,EXCLUDED.source_epoch,EXCLUDED.source_seq,EXCLUDED.rules,EXCLUDED.comparison_rules)
			OR graph_projection_references.expected_hash=EXCLUDED.expected_hash`, string(reference.RepoID), reference.Dataset,
			reference.Applied.Epoch, reference.Applied.Sequence, reference.Rules, reference.ComparisonRules, string(reference.Expected))
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return domain.ErrIntegrity
		}
		return nil
	})
}

func readGraphProjectionReference(ctx context.Context, transaction pgx.Tx, key domain.GraphQualificationKey) (domain.ContentHash, error) {
	var expected domain.ContentHash
	err := transaction.QueryRow(ctx, `SELECT expected_hash FROM graph_projection_references
		WHERE repo_id=$1 AND dataset=$2 AND source_epoch=$3 AND source_seq=$4 AND rules=$5 AND comparison_rules=$6 FOR SHARE`,
		string(key.RepoID), key.Dataset, key.Applied.Epoch, key.Applied.Sequence, key.Rules, key.ComparisonRules).Scan(&expected)
	if err != nil {
		return "", mapNoRows(err)
	}
	if domain.ValidateContentHash(expected) != nil {
		return "", domain.ErrIntegrity
	}
	return expected, nil
}
