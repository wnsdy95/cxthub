//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"time"
)

var _ outbound.GitChangeStore = (*PostgresStore)(nil)

func (s *PostgresStore) EnqueueGitChange(ctx context.Context, j domain.GitChangeJob) (domain.GitChangeJob, error) {
	if err := j.Validate(); err != nil {
		return j, err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return j, err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO git_change_jobs(repo_id,id,payload,state,next_attempt,lease_until,version) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, j.RepoID, j.ID, b, j.State, j.NextAttempt, j.LeaseUntil, j.Version)
	if err != nil {
		return j, err
	}
	old, err := s.GetGitChange(ctx, j.RepoID, j.ID)
	if err == nil && (old.Request != j.Request || old.GitOrigin != j.GitOrigin) {
		err = domain.ErrConflict
	}
	return old, err
}
func (s *PostgresStore) GetGitChange(ctx context.Context, repo domain.ContentHash, id string) (domain.GitChangeJob, error) {
	var raw []byte
	var j domain.GitChangeJob
	err := s.db(ctx).QueryRow(ctx, `SELECT payload FROM git_change_jobs WHERE repo_id=$1 AND id=$2`, repo, id).Scan(&raw)
	if err != nil {
		return j, mapNoRows(err)
	}
	err = json.Unmarshal(raw, &j)
	if err == nil {
		err = j.Validate()
		if j.RepoID != repo || j.ID != id {
			err = domain.ErrIntegrity
		}
	}
	return j, err
}
func (s *PostgresStore) ListGitChanges(ctx context.Context, repo domain.ContentHash, after string, limit int) ([]domain.GitChangeSummary, error) {
	if limit < 1 || limit > 101 {
		return nil, domain.ErrValidation
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT jsonb_build_object('id',id,'request',payload->'request','state',state,'version',version::text,'reason',COALESCE(payload->>'reason',''),'updated_at',payload->'updated_at','coverage',COALESCE(payload->'result'->>'coverage',''),'verified_paths',jsonb_array_length(COALESCE(NULLIF(payload->'result'->'paths','null'::jsonb),'[]'::jsonb)),'unverified_paths',jsonb_array_length(COALESCE(NULLIF(payload->'result'->'unverified_paths','null'::jsonb),'[]'::jsonb))) FROM git_change_jobs WHERE repo_id=$1 AND id>$2 ORDER BY id LIMIT $3`, repo, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.GitChangeSummary{}
	for rows.Next() {
		var raw []byte
		var j domain.GitChangeSummary
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func writeGitChangePG(ctx context.Context, tx pgx.Tx, j domain.GitChangeJob) error {
	if err := j.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE git_change_jobs SET payload=$3,state=$4,next_attempt=$5,lease_until=$6,version=$7 WHERE repo_id=$1 AND id=$2`, j.RepoID, j.ID, b, j.State, j.NextAttempt, j.LeaseUntil, j.Version)
	return err
}
func (s *PostgresStore) ClaimGitChange(ctx context.Context, repo domain.ContentHash, id string, now time.Time, lease time.Duration) (domain.GitChangeJob, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return domain.GitChangeJob{}, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	var j domain.GitChangeJob
	err = tx.QueryRow(ctx, `SELECT payload FROM git_change_jobs WHERE ($1='' OR repo_id=$1) AND ($2='' OR id=$2) AND state IN ('waiting','retrying','running') AND next_attempt<=$3 AND (state<>'running' OR lease_until<=$3) ORDER BY next_attempt,id FOR UPDATE SKIP LOCKED LIMIT 1`, repo, id, now).Scan(&raw)
	if err != nil {
		return j, mapNoRows(err)
	}
	if err = json.Unmarshal(raw, &j); err != nil {
		return j, err
	}
	j, err = j.Claim(now, lease)
	if err != nil {
		return j, err
	}
	if err = writeGitChangePG(ctx, tx, j); err != nil {
		return j, err
	}
	return j, tx.Commit(ctx)
}
func (s *PostgresStore) FinishGitChange(ctx context.Context, j domain.GitChangeJob) error {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	var old domain.GitChangeJob
	err = tx.QueryRow(ctx, `SELECT payload FROM git_change_jobs WHERE repo_id=$1 AND id=$2 FOR UPDATE`, j.RepoID, j.ID).Scan(&raw)
	if err != nil {
		return mapNoRows(err)
	}
	if err = json.Unmarshal(raw, &old); err != nil {
		return err
	}
	if err = old.AcceptFinish(j); err != nil {
		return err
	}
	if err = writeGitChangePG(ctx, tx, j); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *PostgresStore) RetryGitChange(ctx context.Context, repo domain.ContentHash, id string, now time.Time) error {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	var j domain.GitChangeJob
	err = tx.QueryRow(ctx, `SELECT payload FROM git_change_jobs WHERE repo_id=$1 AND id=$2 FOR UPDATE`, repo, id).Scan(&raw)
	if err != nil {
		return mapNoRows(err)
	}
	if err = json.Unmarshal(raw, &j); err != nil {
		return err
	}
	j, err = j.Retry(now)
	if err != nil {
		return err
	}
	if err = writeGitChangePG(ctx, tx, j); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
