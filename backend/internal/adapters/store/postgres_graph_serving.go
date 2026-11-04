//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GraphServingStore = (*PostgresStore)(nil)

type graphServingHead struct {
	domain.GraphQualificationResult
	fence     int64
	completed time.Time
}

func readGraphServingHead(ctx context.Context, database catalogMerkleReader, repo domain.ContentHash) (graphServingHead, error) {
	head := graphServingHead{GraphQualificationResult: domain.GraphQualificationResult{GraphQualificationKey: domain.GraphQualificationKey{
		RepoID: repo, Applied: domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion,
			Scope: domain.RepositoryGraphSourceScope, RepoID: repo}}}}
	err := database.QueryRow(ctx, `SELECT dataset::text,source_epoch::text,source_seq,rules,comparison_rules,
		serving_fence,expected_hash,actual_hash,completed_at FROM graph_serving_heads WHERE repo_id=$1`, string(repo)).
		Scan(&head.Dataset, &head.Applied.Epoch, &head.Applied.Sequence, &head.Rules, &head.ComparisonRules,
			&head.fence, &head.Expected, &head.Actual, &head.completed)
	return head, mapNoRows(err)
}

func graphServingQuarantined(ctx context.Context, database catalogMerkleReader, repo domain.ContentHash, lock bool) (bool, error) {
	var quarantined bool
	query := `SELECT quarantined FROM graph_serving_repairs WHERE repo_id=$1`
	if lock {
		query += ` FOR SHARE`
	}
	err := database.QueryRow(ctx, query, string(repo)).Scan(&quarantined)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return quarantined, err
}

func (s *PostgresStore) promoteGraphServing(ctx context.Context, transaction pgx.Tx, lease domain.GraphQualificationLease, result domain.GraphQualificationResult) error {
	quarantined, err := graphServingQuarantined(ctx, transaction, lease.RepoID, true)
	if err != nil {
		return err
	}
	if quarantined {
		return domain.ErrGraphProjectionFailed
	}
	projection := s.graphProjection()
	parts, err := projection.readReadyPartsFrom(ctx, transaction, lease.RepoID, lease.Dataset, lease.Applied.Sequence)
	if err != nil {
		return err
	}
	if err := projection.validateParts(ctx, lease.RepoID, parts); err != nil {
		return err
	}
	digest, err := domain.GraphViewProjectionManifestDigest(ctx, lease.RepoID, parts)
	if err != nil {
		return err
	}
	if digest != result.Actual {
		return domain.ErrIntegrity
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return err
	}
	var storedBytes int64
	if err := transaction.QueryRow(ctx, `SELECT COALESCE(sum(octet_length(value::text)+octet_length(kind)+octet_length(key)+128),0)
		FROM jsonb_to_recordset($1::jsonb) AS part(kind text,key text,value jsonb)`, raw).Scan(&storedBytes); err != nil {
		return err
	}
	if storedBytes > domain.MaxPendingProjectionOutputBytes {
		return domain.ErrRepositoryViewSourceLimit
	}
	if _, err := transaction.Exec(ctx, `DELETE FROM graph_serving_parts WHERE repo_id=$1`, string(lease.RepoID)); err != nil {
		return err
	}
	command, err := transaction.Exec(ctx, `INSERT INTO graph_serving_heads
		(repo_id,dataset,source_epoch,source_seq,rules,comparison_rules,expected_hash,actual_hash)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(repo_id) DO UPDATE SET dataset=EXCLUDED.dataset,
		source_epoch=EXCLUDED.source_epoch,source_seq=EXCLUDED.source_seq,rules=EXCLUDED.rules,
		comparison_rules=EXCLUDED.comparison_rules,serving_fence=EXCLUDED.serving_fence,
		expected_hash=EXCLUDED.expected_hash,actual_hash=EXCLUDED.actual_hash,completed_at=clock_timestamp()
		WHERE graph_serving_heads.serving_fence<EXCLUDED.serving_fence`, string(lease.RepoID), lease.Dataset, lease.Applied.Epoch,
		lease.Applied.Sequence, lease.Rules, lease.ComparisonRules, string(result.Expected), string(result.Actual))
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return domain.ErrRefConflict
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO graph_serving_parts(repo_id,dataset,source_seq,kind,entity_key,image)
		SELECT $1,$2,$3,kind,key,value FROM jsonb_to_recordset($4::jsonb) AS part(kind text,key text,value jsonb)`,
		string(lease.RepoID), lease.Dataset, lease.Applied.Sequence, raw); err != nil {
		return err
	}
	var unexpired bool
	if err := transaction.QueryRow(ctx, `SELECT $1::timestamptz>clock_timestamp()`, lease.LeaseUntil).Scan(&unexpired); err != nil {
		return err
	}
	if !unexpired {
		return domain.ErrRefConflict
	}
	return nil
}

func (s *PostgresStore) ReadQualifiedGraphProjection(ctx context.Context, repo domain.ContentHash) (domain.GraphProjectionRead, error) {
	return s.readQualifiedGraphProjection(ctx, repo, true)
}

func (s *PostgresStore) ReadQualifiedGraphProjectionStatus(ctx context.Context, repo domain.ContentHash) (domain.GraphProjectionRead, error) {
	return s.readQualifiedGraphProjection(ctx, repo, false)
}

func graphServingCandidateFailed(ctx context.Context, database catalogMerkleReader, repo domain.ContentHash, source projectionSource) (bool, error) {
	var failed bool
	err := database.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM graph_qualification qualification
		JOIN graph_projection_ready ready USING(repo_id) WHERE qualification.repo_id=$1 AND qualification.enabled
		AND ready.source_epoch=$2 AND ready.source_seq<=$3 AND ready.rules=$4 AND (
		(qualification.result_state='mismatch' AND
		 (qualification.result_dataset,qualification.result_epoch,qualification.result_seq,qualification.result_rules,qualification.result_comparison_rules)
		 = (ready.dataset,ready.source_epoch,ready.source_seq,ready.rules,$5::integer)) OR
		(qualification.reason='resource_limit' AND
		 (qualification.attempt_dataset,qualification.attempt_epoch,qualification.attempt_seq,qualification.attempt_rules,qualification.attempt_comparison_rules)
		 = (ready.dataset,ready.source_epoch,ready.source_seq,ready.rules,$5::integer))))`,
		string(repo), source.epoch, source.head, domain.GraphViewProjectionRules, domain.GraphQualificationRules).Scan(&failed)
	return failed, err
}

func (s *PostgresStore) readQualifiedGraphProjection(ctx context.Context, repo domain.ContentHash, includeParts bool) (domain.GraphProjectionRead, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.GraphProjectionRead{}, err
	}
	var result domain.GraphProjectionRead
	outputFailed := false
	err := s.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if !s.InReadOnlyTransaction(bound) {
			return domain.ErrConflict
		}
		database := s.db(bound)
		if err := s.checkRepositoryDocumentIdentity(bound, database, repo, false); err != nil {
			return err
		}
		projection := s.graphProjection()
		source, err := projection.readSource(bound, database, repo, false)
		if err != nil {
			return err
		}
		result.Source = projection.checkpoint(repo, source.epoch, source.head)
		var qualifierEnabled bool
		var workEpoch string
		err = database.QueryRow(bound, `SELECT work.enabled,work.phase='needs_attention',work.source_epoch::text,
			COALESCE(qualification.enabled,false) FROM graph_projection_work work
			LEFT JOIN graph_qualification qualification USING(repo_id) WHERE work.repo_id=$1`, string(repo)).
			Scan(&result.Enabled, &result.NeedsAttention, &workEpoch, &qualifierEnabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		quarantined, err := graphServingQuarantined(bound, database, repo, false)
		if err != nil {
			return err
		}
		result.NeedsAttention = result.NeedsAttention || quarantined
		if !result.Enabled || result.NeedsAttention || !qualifierEnabled || workEpoch != source.epoch {
			return nil
		}
		head, err := readGraphServingHead(bound, database, repo)
		if errors.Is(err, domain.ErrNotFound) {
			result.NeedsAttention, err = graphServingCandidateFailed(bound, database, repo, source)
			return err
		}
		if err != nil {
			return err
		}
		if head.Applied.Epoch != source.epoch || head.Applied.Sequence > source.head ||
			head.Rules != domain.GraphViewProjectionRules || head.ComparisonRules != domain.GraphQualificationRules {
			result.NeedsAttention, err = graphServingCandidateFailed(bound, database, repo, source)
			return err
		}
		if !head.Matched() || head.fence <= 0 || head.completed.IsZero() {
			return domain.ErrIntegrity
		}
		result.Dataset, result.Applied, result.Rules = head.Dataset, head.Applied, head.Rules
		result.ServingFence, result.Qualification = head.fence, &head.GraphQualificationResult
		result.Complete, result.Ready = true, head.Applied.Sequence == source.head
		if !includeParts {
			return nil
		}
		result.Parts, err = s.readGraphServingParts(bound, database, head)
		if err != nil {
			outputFailed = true
		}
		return err
	})
	if err != nil {
		if outputFailed {
			read := projectionRead{PendingProjectionRead: domain.PendingProjectionRead{Enabled: result.Enabled,
				NeedsAttention: result.NeedsAttention, Dataset: result.Dataset, Source: result.Source, Applied: result.Applied},
				complete: result.Complete, rules: result.Rules}
			if failure := graphProjectionOutputCorruption(ctx, repo, read, err); failure != nil {
				failure.Serving, failure.ServingFence = true, result.ServingFence
				result.Parts = nil
				return result, failure
			}
		}
		return domain.GraphProjectionRead{}, err
	}
	return result, nil
}

func (s *PostgresStore) readGraphServingParts(ctx context.Context, database projectionPartsReader, head graphServingHead) ([]domain.GraphProjectionPart, error) {
	var count, size int64
	if err := database.QueryRow(ctx, `SELECT count(*),COALESCE(sum(size),0) FROM
		(SELECT octet_length(image::text)+octet_length(kind)+octet_length(entity_key)+128 AS size
		FROM graph_serving_parts WHERE repo_id=$1 LIMIT $2) bounded`, string(head.RepoID), domain.MaxPendingProjectionOutputRows+1).
		Scan(&count, &size); err != nil {
		return nil, err
	}
	if count > domain.MaxPendingProjectionOutputRows || size > domain.MaxPendingProjectionOutputBytes {
		return nil, domain.ErrRepositoryViewSourceLimit
	}
	projection := s.graphProjection()
	projection.spec.parts = "graph_serving_parts"
	parts, err := projection.readReadyPartsFrom(ctx, database, head.RepoID, head.Dataset, head.Applied.Sequence)
	if err != nil {
		return nil, err
	}
	if int64(len(parts)) != count {
		return nil, domain.ErrIntegrity
	}
	if err := projection.validateParts(ctx, head.RepoID, parts); err != nil {
		return nil, err
	}
	digest, err := domain.GraphViewProjectionManifestDigest(ctx, head.RepoID, parts)
	if err != nil {
		return nil, err
	}
	if digest != head.Actual {
		return nil, domain.ErrIntegrity
	}
	return parts, nil
}
