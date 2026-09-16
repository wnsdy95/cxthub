//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) EnqueuePRJob(ctx context.Context, j domain.PRPromotionJob) (domain.PRPromotionJob, error) {
	if err := j.Validate(); err != nil {
		return j, err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return j, err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO pr_promotion_jobs(repo_id,id,payload,state,created_at,next_attempt,lease_until) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, j.RepoID, j.ID, b, j.State, j.CreatedAt, j.NextAttempt, j.LeaseUntil)
	if err != nil {
		return j, err
	}
	var raw []byte
	err = s.pool.QueryRow(ctx, `SELECT payload FROM pr_promotion_jobs WHERE repo_id=$1 AND id=$2`, j.RepoID, j.ID).Scan(&raw)
	if err != nil {
		return j, mapNoRows(err)
	}
	var old domain.PRPromotionJob
	if err = json.Unmarshal(raw, &old); err != nil {
		return old, err
	}
	if old.PR != j.PR || old.GitOrigin != j.GitOrigin {
		return old, domain.ErrConflict
	}
	return old, nil
}
func (s *PostgresStore) ListPRJobs(ctx context.Context, repo domain.ContentHash) ([]domain.PRPromotionJob, error) {
	rows, err := s.pool.Query(ctx, `SELECT payload FROM pr_promotion_jobs WHERE repo_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.PRPromotionJob{}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var j domain.PRPromotionJob
		if err := json.Unmarshal(b, &j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s *PostgresStore) ClaimPRJob(ctx context.Context, repo domain.ContentHash, id string, now time.Time, lease time.Duration) (domain.PRPromotionJob, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.PRPromotionJob{}, err
	}
	defer tx.Rollback(ctx)
	var b []byte
	err = tx.QueryRow(ctx, `SELECT j.payload FROM pr_promotion_jobs j WHERE ($1='' OR j.repo_id=$1) AND ($2='' OR j.id=$2) AND j.state IN ('waiting','retrying','running') AND j.next_attempt<=$3 AND (j.state<>'running' OR j.lease_until<=$3)
 AND NOT EXISTS(SELECT 1 FROM pr_promotion_jobs p WHERE p.repo_id=j.repo_id AND p.state IN ('waiting','retrying','running') AND (p.created_at,p.id)<(j.created_at,j.id)) ORDER BY j.created_at,j.id FOR UPDATE OF j SKIP LOCKED LIMIT 1`, repo, id, now).Scan(&b)
	if err != nil {
		return domain.PRPromotionJob{}, mapNoRows(err)
	}
	var j domain.PRPromotionJob
	if err = json.Unmarshal(b, &j); err != nil {
		return j, err
	}
	j.State = "running"
	j.Attempts++
	j.Version++
	j.UpdatedAt = now
	j.LeaseUntil = now.Add(lease)
	b, err = json.Marshal(j)
	if err != nil {
		return j, err
	}
	_, err = tx.Exec(ctx, `UPDATE pr_promotion_jobs SET payload=$3,state=$4,lease_until=$5,version=$6 WHERE repo_id=$1 AND id=$2`, j.RepoID, j.ID, b, j.State, j.LeaseUntil, j.Version)
	if err != nil {
		return j, err
	}
	return j, tx.Commit(ctx)
}
func (s *PostgresStore) FinishPRJob(ctx context.Context, j domain.PRPromotionJob) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE pr_promotion_jobs SET payload=$3,state=$4,next_attempt=$5,lease_until=$6 WHERE repo_id=$1 AND id=$2 AND version=$7 AND state='running'`, j.RepoID, j.ID, b, j.State, j.NextAttempt, j.LeaseUntil, j.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}
func (s *PostgresStore) RetryPRJob(ctx context.Context, repo domain.ContentHash, id string, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var b []byte
	if err = tx.QueryRow(ctx, `SELECT payload FROM pr_promotion_jobs WHERE repo_id=$1 AND id=$2 FOR UPDATE`, repo, id).Scan(&b); err != nil {
		return mapNoRows(err)
	}
	var j domain.PRPromotionJob
	if err = json.Unmarshal(b, &j); err != nil {
		return err
	}
	if j.State == "completed" {
		return nil
	}
	if j.State == "running" && j.LeaseUntil.After(now) {
		return domain.ErrConflict
	}
	j.State = "waiting"
	j.NextAttempt = now
	j.UpdatedAt = now
	j.Version++
	j.Attempts = 0
	j.Reason = ""
	b, err = json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE pr_promotion_jobs SET payload=$3,state=$4,next_attempt=$5,version=$6 WHERE repo_id=$1 AND id=$2`, repo, id, b, j.State, j.NextAttempt, j.Version)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) GetPRJob(ctx context.Context, repo domain.ContentHash, id string) (domain.PRPromotionJob, error) {
	var j domain.PRPromotionJob
	var b []byte
	err := s.pool.QueryRow(ctx, `SELECT payload FROM pr_promotion_jobs WHERE repo_id=$1 AND id=$2`, repo, id).Scan(&b)
	if err != nil {
		return j, mapNoRows(err)
	}
	err = json.Unmarshal(b, &j)
	return j, err
}
