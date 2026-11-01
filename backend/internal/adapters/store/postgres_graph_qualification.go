//go:build postgres

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GraphQualificationStore = (*PostgresStore)(nil)

func (s *PostgresStore) graphQualificationWriteContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.InTransaction(ctx) || outbound.DocReadOnly(ctx) {
		return domain.ErrConflict
	}
	return nil
}

func (s *PostgresStore) lockGraphQualificationSource(ctx context.Context, transaction pgx.Tx, repo domain.ContentHash, skipLocked bool) (projectionWork, projectionSource, error) {
	if err := s.checkRepositoryDocumentIdentity(ctx, transaction, repo, true); err != nil {
		return projectionWork{}, projectionSource{}, err
	}
	query := `SELECT ` + projectionWorkColumns + ` FROM graph_projection_work work WHERE repo_id=$1 FOR UPDATE`
	if skipLocked {
		query += ` SKIP LOCKED`
	}
	work, err := scanProjectionWork(transaction.QueryRow(ctx, query, string(repo)))
	if err != nil {
		return work, projectionSource{}, err
	}
	source, err := s.graphProjection().readSource(ctx, transaction, repo, true)
	return work, source, err
}

func graphQualificationReady(ctx context.Context, transaction pgx.Tx, work projectionWork, source projectionSource, current bool) (domain.GraphQualificationKey, error) {
	if !work.enabled || work.Phase == "needs_attention" || work.Epoch != source.epoch {
		return domain.GraphQualificationKey{}, domain.ErrNotFound
	}
	key := domain.GraphQualificationKey{RepoID: work.RepoID, ComparisonRules: domain.GraphQualificationRules,
		Applied: domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion,
			Scope: domain.RepositoryGraphSourceScope, RepoID: work.RepoID}}
	err := transaction.QueryRow(ctx, `SELECT dataset::text,source_epoch::text,source_seq,rules
		FROM graph_projection_ready WHERE repo_id=$1`, string(work.RepoID)).Scan(&key.Dataset, &key.Applied.Epoch, &key.Applied.Sequence, &key.Rules)
	if err != nil {
		return domain.GraphQualificationKey{}, mapNoRows(err)
	}
	if key.Validate() != nil || key.Applied.Epoch != source.epoch || key.Applied.Sequence > source.head || (current && key.Applied.Sequence != source.head) {
		return domain.GraphQualificationKey{}, domain.ErrNotFound
	}
	return key, nil
}

func (s *PostgresStore) ConfigureGraphQualification(ctx context.Context, repo domain.ContentHash, enabled bool) error {
	if err := s.graphQualificationWriteContext(ctx); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	transaction, err := s.graphProjection().begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if _, _, err := s.lockGraphQualificationSource(ctx, transaction, repo, false); err != nil {
		return err
	}
	_, err = transaction.Exec(ctx, `INSERT INTO graph_qualification(repo_id,enabled) VALUES($1,$2)
		ON CONFLICT(repo_id) DO UPDATE SET enabled=EXCLUDED.enabled,fence=graph_qualification.fence+1,lease_until=NULL
		WHERE graph_qualification.enabled<>EXCLUDED.enabled`, string(repo), enabled)
	if err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func (s *PostgresStore) ClaimGraphQualification(ctx context.Context, repo domain.ContentHash, duration time.Duration) (domain.GraphQualificationLease, error) {
	if err := s.graphQualificationWriteContext(ctx); err != nil {
		return domain.GraphQualificationLease{}, err
	}
	if domain.ValidateContentHash(repo) != nil || duration < time.Second || duration > 5*time.Minute {
		return domain.GraphQualificationLease{}, domain.ErrValidation
	}
	transaction, err := s.graphProjection().begin(ctx)
	if err != nil {
		return domain.GraphQualificationLease{}, err
	}
	defer rollbackPG(transaction)
	work, source, err := s.lockGraphQualificationSource(ctx, transaction, repo, true)
	if err != nil {
		return domain.GraphQualificationLease{}, err
	}
	key, err := graphQualificationReady(ctx, transaction, work, source, false)
	if err != nil {
		return domain.GraphQualificationLease{}, err
	}
	lease := domain.GraphQualificationLease{GraphQualificationKey: key}
	lease.ReferenceExpected, err = readGraphProjectionReference(ctx, transaction, key)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.GraphQualificationLease{}, err
	}
	if lease.ReferenceExpected == "" && key.Applied.Sequence != source.head {
		return domain.GraphQualificationLease{}, domain.ErrNotFound
	}
	err = transaction.QueryRow(ctx, `UPDATE graph_qualification qualification SET fence=fence+1,
		lease_until=clock_timestamp()+$8::bigint*interval '1 microsecond',next_attempt=clock_timestamp()+interval '60 seconds',reason='',
		attempt_dataset=$2,attempt_epoch=$3,attempt_seq=$4,attempt_rules=$5,attempt_comparison_rules=$6,attempt_work_dataset=$7,
		attempt_reference_hash=NULLIF($9::text,'')
		WHERE repo_id=$1 AND enabled AND next_attempt<=clock_timestamp() AND (lease_until IS NULL OR lease_until<=clock_timestamp())
		AND ((result_dataset,result_epoch,result_seq,result_rules,result_comparison_rules)
			IS DISTINCT FROM ($2::uuid,$3::uuid,$4::bigint,$5::integer,$6::integer)
			OR (result_state='matched' AND NOT EXISTS(SELECT 1 FROM graph_serving_heads head WHERE head.repo_id=$1
				AND (head.dataset,head.source_epoch,head.source_seq,head.rules,head.comparison_rules)
				= ($2::uuid,$3::uuid,$4::bigint,$5::integer,$6::integer)
				AND head.expected_hash=qualification.expected_hash AND head.actual_hash=qualification.actual_hash)))
		AND (reason<>'resource_limit' OR (attempt_dataset,attempt_epoch,attempt_seq,attempt_rules,attempt_comparison_rules)
			IS DISTINCT FROM ($2::uuid,$3::uuid,$4::bigint,$5::integer,$6::integer))
		AND NOT EXISTS(SELECT 1 FROM graph_serving_repairs WHERE repo_id=$1 AND quarantined)
		RETURNING fence,lease_until`, string(repo), key.Dataset, key.Applied.Epoch, key.Applied.Sequence, key.Rules, key.ComparisonRules, work.Dataset, duration.Microseconds(), string(lease.ReferenceExpected)).
		Scan(&lease.Fence, &lease.LeaseUntil)
	if err != nil {
		return domain.GraphQualificationLease{}, mapNoRows(err)
	}
	if err := lease.Validate(); err != nil {
		return domain.GraphQualificationLease{}, domain.ErrIntegrity
	}
	if err := transaction.Commit(ctx); err != nil {
		return domain.GraphQualificationLease{}, err
	}
	return lease, nil
}

const graphQualificationLeasePredicate = `repo_id=$1 AND enabled AND fence=$2 AND lease_until=$3 AND lease_until>clock_timestamp()
	AND attempt_dataset=$4 AND attempt_epoch=$5 AND attempt_seq=$6 AND attempt_rules=$7 AND attempt_comparison_rules=$8
	AND attempt_reference_hash IS NOT DISTINCT FROM NULLIF($9::text,'')`

func graphQualificationLeaseArgs(lease domain.GraphQualificationLease) []any {
	return []any{string(lease.RepoID), lease.Fence, lease.LeaseUntil, lease.Dataset, lease.Applied.Epoch,
		lease.Applied.Sequence, lease.Rules, lease.ComparisonRules, string(lease.ReferenceExpected)}
}

func graphQualificationConflict(err error) error {
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrRefConflict
	}
	return err
}

func (s *PostgresStore) FinishGraphQualification(ctx context.Context, lease domain.GraphQualificationLease, result domain.GraphQualificationResult) (err error) {
	defer func() {
		var failure *pgconn.PgError
		if errors.As(err, &failure) && failure.Code == "40001" {
			err = errors.Join(domain.ErrRefConflict, err)
		}
	}()
	if err := s.graphQualificationWriteContext(ctx); err != nil {
		return err
	}
	if lease.Validate() != nil || result.Validate() != nil || lease.GraphQualificationKey != result.GraphQualificationKey {
		return domain.ErrValidation
	}
	transaction, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	work, source, err := s.lockGraphQualificationSource(ctx, transaction, lease.RepoID, false)
	if err != nil {
		return graphQualificationConflict(err)
	}
	key, err := graphQualificationReady(ctx, transaction, work, source, false)
	if err != nil {
		return graphQualificationConflict(err)
	}
	if key != lease.GraphQualificationKey {
		return domain.ErrRefConflict
	}
	if lease.ReferenceExpected != "" {
		expected, err := readGraphProjectionReference(ctx, transaction, key)
		if err != nil {
			return graphQualificationConflict(err)
		}
		if expected != lease.ReferenceExpected {
			return domain.ErrRefConflict
		}
		if result.Expected != expected {
			return domain.ErrIntegrity
		}
	}
	args := append(graphQualificationLeaseArgs(lease), work.Dataset, string(result.Expected), string(result.Actual))
	command, err := transaction.Exec(ctx, `UPDATE graph_qualification SET lease_until=NULL,reason='',
		result_dataset=attempt_dataset,result_epoch=attempt_epoch,result_seq=attempt_seq,result_rules=attempt_rules,
		result_comparison_rules=attempt_comparison_rules,expected_hash=$11,actual_hash=$12,
		result_state=CASE WHEN $11::text=$12::text THEN 'matched' ELSE 'mismatch' END,completed_at=clock_timestamp(),
		next_attempt=CASE WHEN $11::text=$12::text THEN clock_timestamp() ELSE next_attempt END
		WHERE `+graphQualificationLeasePredicate+` AND attempt_work_dataset=$10`, args...)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return domain.ErrRefConflict
	}
	if result.Matched() {
		if err := s.promoteGraphServing(ctx, transaction, lease, result); err != nil {
			return err
		}
	}
	return transaction.Commit(ctx)
}

func (s *PostgresStore) FailGraphQualification(ctx context.Context, lease domain.GraphQualificationLease, reason string) error {
	if err := s.graphQualificationWriteContext(ctx); err != nil {
		return err
	}
	if lease.Validate() != nil || (reason != "not_current" && reason != "read_error" && reason != "resource_limit") {
		return domain.ErrValidation
	}
	transaction, err := s.graphProjection().begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if _, _, err := s.lockGraphQualificationSource(ctx, transaction, lease.RepoID, false); err != nil {
		return graphQualificationConflict(err)
	}
	command, err := transaction.Exec(ctx, `UPDATE graph_qualification SET lease_until=NULL,reason=$10,
		next_attempt=GREATEST(next_attempt,clock_timestamp()+interval '60 seconds') WHERE `+graphQualificationLeasePredicate,
		append(graphQualificationLeaseArgs(lease), reason)...)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return domain.ErrRefConflict
	}
	return transaction.Commit(ctx)
}

func (s *PostgresStore) ReadGraphQualification(ctx context.Context, key domain.GraphQualificationKey) (domain.GraphQualificationResult, error) {
	if err := key.Validate(); err != nil {
		return domain.GraphQualificationResult{}, err
	}
	var result domain.GraphQualificationResult
	err := s.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if !s.InReadOnlyTransaction(bound) {
			return domain.ErrConflict
		}
		if err := s.checkRepositoryDocumentIdentity(bound, s.db(bound), key.RepoID, false); err != nil {
			return err
		}
		result.GraphQualificationKey = domain.GraphQualificationKey{RepoID: key.RepoID,
			Applied: domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion,
				Scope: domain.RepositoryGraphSourceScope, RepoID: key.RepoID}}
		var state string
		var completed time.Time
		err := s.db(bound).QueryRow(bound, `SELECT qualification.result_dataset::text,qualification.result_epoch::text,
			qualification.result_seq,qualification.result_rules,qualification.result_comparison_rules,
			qualification.expected_hash,qualification.actual_hash,qualification.result_state,qualification.completed_at
			FROM graph_qualification qualification JOIN graph_projection_work work USING(repo_id)
			WHERE qualification.repo_id=$1 AND qualification.enabled AND work.enabled AND qualification.result_dataset=$2
			AND qualification.result_epoch=$3 AND qualification.result_seq=$4 AND qualification.result_rules=$5 AND qualification.result_comparison_rules=$6`,
			string(key.RepoID), key.Dataset, key.Applied.Epoch, key.Applied.Sequence, key.Rules, key.ComparisonRules).
			Scan(&result.Dataset, &result.Applied.Epoch, &result.Applied.Sequence, &result.Rules, &result.ComparisonRules,
				&result.Expected, &result.Actual, &state, &completed)
		if errors.Is(err, pgx.ErrNoRows) {
			var limited bool
			if err := s.db(bound).QueryRow(bound, `SELECT EXISTS(SELECT 1 FROM graph_qualification qualification
				JOIN graph_projection_work work USING(repo_id)
				WHERE qualification.repo_id=$1 AND qualification.enabled AND work.enabled AND qualification.reason='resource_limit'
				AND qualification.attempt_dataset=$2 AND qualification.attempt_epoch=$3 AND qualification.attempt_seq=$4
				AND qualification.attempt_rules=$5 AND qualification.attempt_comparison_rules=$6)`,
				string(key.RepoID), key.Dataset, key.Applied.Epoch, key.Applied.Sequence, key.Rules, key.ComparisonRules).Scan(&limited); err != nil {
				return err
			}
			if limited {
				return domain.ErrGraphProjectionFailed
			}
		}
		if err != nil {
			return mapNoRows(err)
		}
		if result.Validate() != nil || result.GraphQualificationKey != key || completed.IsZero() ||
			(state != "matched" && state != "mismatch") || (state == "matched") != result.Matched() {
			return domain.ErrIntegrity
		}
		return nil
	})
	if err != nil {
		return domain.GraphQualificationResult{}, err
	}
	return result, nil
}
