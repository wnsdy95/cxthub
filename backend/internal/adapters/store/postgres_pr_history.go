//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.PRHistoryStore = (*PostgresStore)(nil)

func (s *PostgresStore) ListPRHistoryJobs(ctx context.Context, repo domain.ContentHash, origin, jobOrigin string, providerRepositoryID int64, now time.Time, limit int) ([]domain.PRPromotionJob, error) {
	if err := validatePRHistoryJobsQuery(repo, origin, jobOrigin, providerRepositoryID, now, limit); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT j.id,j.payload,c.payload,COALESCE(c.version,0)
 FROM pr_promotion_jobs j LEFT JOIN pr_history_checkpoints c
 ON c.repo_id=j.repo_id AND c.origin=$2 AND c.provider_repository_id=$4 AND c.number::text=j.payload->'pr'->>'number'
 WHERE j.repo_id=$1 AND j.payload->>'git_origin'=$3
 AND (c.repo_id IS NULL OR (c.payload->>'next_attempt')::timestamptz<=$5)
 ORDER BY COALESCE((c.payload->>'next_attempt')::timestamptz,j.created_at),j.id LIMIT $6`, repo, origin, jobOrigin, providerRepositoryID, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.PRPromotionJob{}
	for rows.Next() {
		var id string
		var jobRaw, checkpointRaw []byte
		var checkpointVersion int64
		if err := rows.Scan(&id, &jobRaw, &checkpointRaw, &checkpointVersion); err != nil {
			return nil, err
		}
		job, err := decodePRHistory[domain.PRPromotionJob](jobRaw)
		if err != nil {
			return nil, err
		}
		if job.ID != id || job.RepoID != repo || job.GitOrigin != jobOrigin {
			return nil, domain.ErrIntegrity
		}
		if checkpointRaw != nil {
			checkpoint, err := decodePRHistory[domain.PRHistoryCheckpoint](checkpointRaw)
			if err != nil {
				return nil, err
			}
			if err := validatePRCheckpointScope(checkpoint, repo, origin, providerRepositoryID, job.PR.Number); err != nil {
				return nil, err
			}
			if checkpoint.Version != checkpointVersion {
				return nil, domain.ErrIntegrity
			}
			if checkpoint.NextAttempt.After(now) {
				continue
			}
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

func readPRCheckpointPG(ctx context.Context, database catalogMerkleReader, repo domain.ContentHash, origin string, providerRepositoryID int64, number int, lock bool) (domain.PRHistoryCheckpoint, error) {
	query := `SELECT payload,version FROM pr_history_checkpoints WHERE repo_id=$1 AND origin=$2 AND provider_repository_id=$3 AND number=$4`
	if lock {
		query += ` FOR UPDATE`
	}
	var raw []byte
	var version int64
	if err := database.QueryRow(ctx, query, repo, origin, providerRepositoryID, number).Scan(&raw, &version); err != nil {
		return domain.PRHistoryCheckpoint{}, mapNoRows(err)
	}
	checkpoint, err := decodePRHistory[domain.PRHistoryCheckpoint](raw)
	if err != nil {
		return domain.PRHistoryCheckpoint{}, err
	}
	if err = validatePRCheckpointScope(checkpoint, repo, origin, providerRepositoryID, number); err != nil {
		return domain.PRHistoryCheckpoint{}, err
	}
	if checkpoint.Version != version {
		return domain.PRHistoryCheckpoint{}, domain.ErrIntegrity
	}
	return checkpoint, nil
}

func (s *PostgresStore) GetPRHistoryCheckpoint(ctx context.Context, repo domain.ContentHash, origin string, providerRepositoryID int64, number int) (domain.PRHistoryCheckpoint, error) {
	if err := validatePRHistoryScope(repo, origin, number); err != nil {
		return domain.PRHistoryCheckpoint{}, err
	}
	if providerRepositoryID <= 0 {
		return domain.PRHistoryCheckpoint{}, domain.ErrValidation
	}
	return readPRCheckpointPG(ctx, s.db(ctx), repo, origin, providerRepositoryID, number, false)
}

func (s *PostgresStore) SavePRHistoryCheckpoint(ctx context.Context, next domain.PRHistoryCheckpoint, expectedVersion int64) error {
	if err := validatePRCheckpointVersion(next, expectedVersion); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	if err = s.checkRepositoryDocumentIdentity(ctx, tx, next.RepoID, true); err != nil {
		return err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if expectedVersion == 0 {
		result, err := tx.Exec(ctx, `INSERT INTO pr_history_checkpoints(repo_id,origin,provider_repository_id,number,version,payload) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, next.RepoID, next.GitOrigin, next.ProviderRepositoryID, next.PR.Number, next.Version, raw)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return domain.ErrConflict
		}
	} else {
		old, err := readPRCheckpointPG(ctx, tx, next.RepoID, next.GitOrigin, next.ProviderRepositoryID, next.PR.Number, true)
		if err == domain.ErrNotFound {
			return domain.ErrConflict
		}
		if err != nil {
			return err
		}
		if err = acceptPRCheckpoint(old, next, expectedVersion); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE pr_history_checkpoints SET version=$5,payload=$6 WHERE repo_id=$1 AND origin=$2 AND provider_repository_id=$3 AND number=$4 AND version=$7`, next.RepoID, next.GitOrigin, next.ProviderRepositoryID, next.PR.Number, next.Version, raw, expectedVersion)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return domain.ErrConflict
		}
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) ListPRRefObservations(ctx context.Context, repo domain.ContentHash, origin string, providerRepositoryID int64, number int, afterID string, limit int) ([]domain.GitRefObservation, error) {
	if err := validatePRObservationQuery(repo, origin, providerRepositoryID, number, afterID, limit); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT id,payload FROM git_ref_observations WHERE repo_id=$1 AND payload->>'git_origin'=$2 AND payload->'pull_request'->>'repository_id'=$3 AND payload->'pull_request'->>'number'=$4 AND id>$5 AND payload->>'source' IN ('github-pull-request','github-pr-reconciliation') ORDER BY id LIMIT $6`, repo, origin, strconv.FormatInt(providerRepositoryID, 10), strconv.Itoa(number), afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.GitRefObservation{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		observation, err := decodePRHistory[domain.GitRefObservation](raw)
		if err != nil {
			return nil, err
		}
		if observation.ID != id || observation.ID <= afterID || !matchesPRObservation(observation, repo, origin, providerRepositoryID, number) {
			return nil, domain.ErrIntegrity
		}
		result = append(result, observation)
	}
	return result, rows.Err()
}

func (s *PostgresStore) PutPRParticipation(ctx context.Context, participation domain.PRParticipation) error {
	if err := validatePRParticipationWrite(participation); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	if err = s.checkRepositoryDocumentIdentity(ctx, tx, participation.RepoID, true); err != nil {
		return err
	}
	raw, err := json.Marshal(participation)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO pr_participations(repo_id,id,origin,provider_repository_id,number,payload) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, participation.RepoID, participation.ID, participation.GitOrigin, participation.ProviderRepositoryID, participation.Number, raw); err != nil {
		return err
	}
	var stored []byte
	var origin string
	var providerRepositoryID int64
	var number int
	if err = tx.QueryRow(ctx, `SELECT origin,provider_repository_id,number,payload FROM pr_participations WHERE repo_id=$1 AND id=$2`, participation.RepoID, participation.ID).Scan(&origin, &providerRepositoryID, &number, &stored); err != nil {
		return mapNoRows(err)
	}
	old, err := decodePRHistory[domain.PRParticipation](stored)
	if err != nil {
		return err
	}
	if old != participation || old.GitOrigin != origin || old.ProviderRepositoryID != providerRepositoryID || old.Number != number {
		return domain.ErrIntegrity
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) ListPRParticipations(ctx context.Context, repo domain.ContentHash, origin string, number int) ([]domain.PRParticipation, error) {
	if err := validatePRHistoryScope(repo, origin, number); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT id,provider_repository_id,payload FROM pr_participations WHERE repo_id=$1 AND origin=$2 AND number=$3 ORDER BY id`, repo, origin, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.PRParticipation{}
	for rows.Next() {
		var id string
		var providerRepositoryID int64
		var raw []byte
		if err := rows.Scan(&id, &providerRepositoryID, &raw); err != nil {
			return nil, err
		}
		participation, err := decodePRHistory[domain.PRParticipation](raw)
		if err != nil {
			return nil, err
		}
		if participation.ID != id || participation.RepoID != repo || participation.GitOrigin != origin || participation.ProviderRepositoryID != providerRepositoryID || participation.Number != number {
			return nil, domain.ErrIntegrity
		}
		result = append(result, participation)
	}
	return result, rows.Err()
}
