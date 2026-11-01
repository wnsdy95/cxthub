//go:build postgres

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) RequestGraphServingRepair(ctx context.Context, failure domain.GraphProjectionCorruption) error {
	if err := s.graphQualificationWriteContext(ctx); err != nil {
		return err
	}
	if failure.Validate() != nil || !failure.Serving {
		return domain.ErrValidation
	}
	transaction, err := s.graphProjection().begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	work, source, err := s.lockGraphQualificationSource(ctx, transaction, failure.RepoID, false)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !work.enabled || work.Epoch != source.epoch || source.epoch != failure.Applied.Epoch || source.head < failure.Applied.Sequence {
		return nil
	}
	var qualifierEnabled bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM graph_qualification WHERE repo_id=$1 AND enabled)`, string(failure.RepoID)).Scan(&qualifierEnabled); err != nil {
		return err
	}
	if !qualifierEnabled {
		return nil
	}
	head, err := readGraphServingHead(ctx, transaction, failure.RepoID)
	missing := errors.Is(err, domain.ErrNotFound)
	if err != nil && !missing {
		return err
	}
	if !missing && (head.Dataset != failure.Dataset || head.Applied != failure.Applied || head.Rules != failure.Rules || head.fence != failure.ServingFence) {
		return nil
	}
	var rebuilds []time.Time
	var quarantined, sameReceipt bool
	err = transaction.QueryRow(ctx, `SELECT rebuilds,quarantined,
		dataset=$2 AND source_epoch=$3 AND source_seq=$4 AND rules=$5 AND serving_fence=$6
		FROM graph_serving_repairs WHERE repo_id=$1`, string(failure.RepoID), failure.Dataset, failure.Applied.Epoch,
		failure.Applied.Sequence, failure.Rules, failure.ServingFence).Scan(&rebuilds, &quarantined, &sameReceipt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if sameReceipt {
		if _, err := transaction.Exec(ctx, `UPDATE graph_serving_repairs
			SET request_count=LEAST(request_count,9223372036854775806)+1,last_requested_at=clock_timestamp() WHERE repo_id=$1`, string(failure.RepoID)); err != nil {
			return err
		}
		return transaction.Commit(ctx)
	}
	if missing || quarantined || work.Phase == "needs_attention" {
		return nil
	}
	if !head.Matched() || head.completed.IsZero() {
		return domain.ErrIntegrity
	}
	var now time.Time
	if err := transaction.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	rebuilds, quarantined = graphProjectionRepairWindow(rebuilds, now)
	_, err = transaction.Exec(ctx, `INSERT INTO graph_serving_repairs
		(repo_id,dataset,source_epoch,source_seq,rules,serving_fence,reason,request_count,last_requested_at,window_started_at,rebuilds,quarantined)
		VALUES($1,$2,$3,$4,$5,$6,$7,1,$8,$9,$10,$11) ON CONFLICT(repo_id) DO UPDATE SET
		dataset=EXCLUDED.dataset,source_epoch=EXCLUDED.source_epoch,source_seq=EXCLUDED.source_seq,rules=EXCLUDED.rules,
		serving_fence=EXCLUDED.serving_fence,reason=EXCLUDED.reason,
		request_count=LEAST(graph_serving_repairs.request_count,9223372036854775806)+1,
		last_requested_at=EXCLUDED.last_requested_at,window_started_at=EXCLUDED.window_started_at,
		rebuilds=EXCLUDED.rebuilds,quarantined=EXCLUDED.quarantined`, string(failure.RepoID), failure.Dataset,
		failure.Applied.Epoch, failure.Applied.Sequence, failure.Rules, failure.ServingFence, graphProjectionRepairReason(failure.Cause),
		now, rebuilds[0], rebuilds, quarantined)
	if err != nil {
		return err
	}
	_, err = transaction.Exec(ctx, `UPDATE graph_qualification SET result_dataset=NULL,result_epoch=NULL,result_seq=NULL,
		result_rules=NULL,result_comparison_rules=NULL,expected_hash=NULL,actual_hash=NULL,result_state=NULL,completed_at=NULL
		WHERE repo_id=$1 AND result_dataset=$2 AND result_epoch=$3 AND result_seq=$4 AND result_rules=$5
		AND result_comparison_rules=$6 AND result_state='matched' AND expected_hash=$7 AND actual_hash=$8`,
		string(failure.RepoID), head.Dataset, head.Applied.Epoch, head.Applied.Sequence, head.Rules, head.ComparisonRules, string(head.Expected), string(head.Actual))
	if err != nil {
		return err
	}
	command, err := transaction.Exec(ctx, `DELETE FROM graph_serving_heads WHERE repo_id=$1 AND dataset=$2 AND source_epoch=$3
		AND source_seq=$4 AND rules=$5 AND serving_fence=$6`, string(failure.RepoID), failure.Dataset, failure.Applied.Epoch,
		failure.Applied.Sequence, failure.Rules, failure.ServingFence)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return domain.ErrRefConflict
	}
	return transaction.Commit(ctx)
}
