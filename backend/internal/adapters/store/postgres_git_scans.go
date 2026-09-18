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

var _ outbound.GitScanStore = (*PostgresStore)(nil)

func (s *PostgresStore) EnqueueGitScan(ctx context.Context, j domain.GitScanJob) error {
	if err := j.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO git_scan_jobs(repo_id,id,payload,state,next_attempt,lease_until) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, j.RepoID, j.ID, b, j.State, j.NextAttempt, j.LeaseUntil)
	return err
}
func (s *PostgresStore) GetGitScan(ctx context.Context, repo domain.ContentHash, id string) (domain.GitScanJob, error) {
	var b []byte
	var j domain.GitScanJob
	err := s.db(ctx).QueryRow(ctx, `SELECT payload FROM git_scan_jobs WHERE repo_id=$1 AND id=$2`, repo, id).Scan(&b)
	if err != nil {
		return j, mapNoRows(err)
	}
	err = json.Unmarshal(b, &j)
	if err == nil {
		err = j.Validate()
		if j.RepoID != repo || j.ID != id {
			err = domain.ErrIntegrity
		}
	}
	return j, err
}
func (s *PostgresStore) ListGitScans(ctx context.Context, repo domain.ContentHash, after string, limit int) ([]domain.GitScanJob, error) {
	if limit < 1 || limit > 101 {
		return nil, domain.ErrValidation
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT payload FROM git_scan_jobs WHERE repo_id=$1 AND id>$2 ORDER BY id LIMIT $3`, repo, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.GitScanJob{}
	for rows.Next() {
		var b []byte
		var j domain.GitScanJob
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func writeGitScanPG(ctx context.Context, tx pgx.Tx, j domain.GitScanJob) error {
	if err := j.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE git_scan_jobs SET payload=$3,state=$4,next_attempt=$5,lease_until=$6 WHERE repo_id=$1 AND id=$2`, j.RepoID, j.ID, b, j.State, j.NextAttempt, j.LeaseUntil)
	return err
}
func (s *PostgresStore) ClaimGitScan(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.GitScanJob, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return domain.GitScanJob{}, err
	}
	defer tx.Rollback(ctx)
	var b []byte
	var j domain.GitScanJob
	err = tx.QueryRow(ctx, `SELECT payload FROM git_scan_jobs WHERE ($2='' OR repo_id=$2) AND (state IN ('waiting','running','retrying') OR (state='completed' AND NOT coalesce((payload->>'tree_indexed')::boolean,false))) AND next_attempt<=$1 AND (state<>'running' OR lease_until<=$1) ORDER BY next_attempt,id FOR UPDATE SKIP LOCKED LIMIT 1`, now, repo).Scan(&b)
	if err != nil {
		return j, mapNoRows(err)
	}
	if err = json.Unmarshal(b, &j); err != nil {
		return j, err
	}
	j, err = j.Claim(now, lease)
	if err != nil {
		return j, err
	}
	if err = writeGitScanPG(ctx, tx, j); err != nil {
		return j, err
	}
	return j, tx.Commit(ctx)
}
func (s *PostgresStore) FinishGitScan(ctx context.Context, p domain.GitScanFinish) error {
	if err := p.Validate(); err != nil {
		return err
	}
	// Use the repository transaction handle, then a savepoint for this fence.
	// Dependent queues and immutable index rows are rolled back with the cursor.
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var b []byte
	var old domain.GitScanJob
	if err = tx.QueryRow(ctx, `SELECT payload FROM git_scan_jobs WHERE repo_id=$1 AND id=$2 FOR UPDATE`, p.Job.RepoID, p.Job.ID).Scan(&b); err != nil {
		return mapNoRows(err)
	}
	if err = json.Unmarshal(b, &old); err != nil {
		return err
	}
	if err = p.ValidateFor(old); err != nil {
		return err
	}
	if p.Tree != nil {
		if err = writeGitTreePG(ctx, tx, p.Job.RepoID, p.Job.GitOrigin, *p.Tree); err != nil {
			return err
		}
	}
	for _, d := range p.Deltas {
		b, err = json.Marshal(d)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO git_commit_deltas(repo_id,id,origin,commit_oid,parent_oid,payload,keys,inverse_keys) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, d.RepoID, d.ID, d.GitOrigin, d.Delta.Commit, d.Delta.Parent, b, d.Keys, d.InverseKeys)
		if err != nil {
			return err
		}
		var same bool
		if err = tx.QueryRow(ctx, `SELECT payload=$3::jsonb FROM git_commit_deltas WHERE repo_id=$1 AND id=$2`, d.RepoID, d.ID, b).Scan(&same); err != nil {
			return err
		}
		if !same {
			return domain.ErrIntegrity
		}
	}
	for _, j := range p.Parents {
		b, err = json.Marshal(j)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO git_scan_jobs(repo_id,id,payload,state,next_attempt,lease_until) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, j.RepoID, j.ID, b, j.State, j.NextAttempt, j.LeaseUntil)
		if err != nil {
			return err
		}
	}
	for _, j := range p.Changes {
		b, err = json.Marshal(j)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO git_change_jobs(repo_id,id,payload,state,next_attempt,lease_until,version) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, j.RepoID, j.ID, b, j.State, j.NextAttempt, j.LeaseUntil, j.Version)
		if err != nil {
			return err
		}
	}
	if err = writeGitScanPG(ctx, tx, p.Job); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *PostgresStore) RetryGitScan(ctx context.Context, repo domain.ContentHash, id string, now time.Time) error {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var b []byte
	var j domain.GitScanJob
	if err = tx.QueryRow(ctx, `SELECT payload FROM git_scan_jobs WHERE repo_id=$1 AND id=$2 FOR UPDATE`, repo, id).Scan(&b); err != nil {
		return mapNoRows(err)
	}
	if err = json.Unmarshal(b, &j); err != nil {
		return err
	}
	j, err = j.Retry(now)
	if err != nil {
		return err
	}
	if err = writeGitScanPG(ctx, tx, j); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *PostgresStore) FindGitInverses(ctx context.Context, repo domain.ContentHash, origin, commit, after string, limit int) ([]domain.GitInverseCandidate, error) {
	if limit < 1 || limit > 51 {
		return nil, domain.ErrValidation
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT a.id||b.id,b.commit_oid,b.parent_oid,a.commit_oid,a.parent_oid FROM git_commit_deltas a JOIN git_commit_deltas b ON b.repo_id=a.repo_id AND b.origin=a.origin AND b.commit_oid<>a.commit_oid AND b.keys && a.inverse_keys WHERE a.repo_id=$1 AND a.origin=$2 AND a.commit_oid=$3 AND a.id||b.id>$4 ORDER BY a.id||b.id LIMIT $5`, repo, origin, commit, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.GitInverseCandidate{}
	for rows.Next() {
		var c domain.GitInverseCandidate
		if err = rows.Scan(&c.Cursor, &c.Request.Target, &c.Request.TargetParent, &c.Request.Commit, &c.Request.Parent); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *PostgresStore) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, commit, parent string) (domain.GitCommitDelta, error) {
	var b []byte
	var r domain.GitDeltaRecord
	// Blank parent is resolved only when exactly one comparison is cached.
	rows, err := s.db(ctx).Query(ctx, `SELECT payload FROM git_commit_deltas WHERE repo_id=$1 AND origin=$2 AND commit_oid=$3 AND ($4='' OR parent_oid=$4) LIMIT 2`, repo, origin, commit, parent)
	if err != nil {
		return r.Delta, err
	}
	defer rows.Close()
	if !rows.Next() {
		return r.Delta, domain.ErrNotFound
	}
	if err = rows.Scan(&b); err != nil {
		return r.Delta, err
	}
	if rows.Next() {
		return r.Delta, domain.ErrNotFound
	}
	if err = rows.Err(); err != nil {
		return r.Delta, err
	}
	if err = json.Unmarshal(b, &r); err != nil {
		return r.Delta, err
	}
	return r.Delta, r.Validate()
}
func (s *PostgresStore) RecordGitRefObservation(ctx context.Context, o domain.GitRefObservation, now time.Time) error {
	if err := o.Validate(); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = putGitObservationPG(ctx, tx, o, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func putGitObservationPG(ctx context.Context, tx pgx.Tx, o domain.GitRefObservation, now time.Time) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO git_ref_observations(repo_id,id,payload,observed_at) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, o.RepoID, o.ID, b, now)
	if err != nil {
		return err
	}
	var same bool
	if err = tx.QueryRow(ctx, `SELECT payload=$3::jsonb FROM git_ref_observations WHERE repo_id=$1 AND id=$2`, o.RepoID, o.ID, b).Scan(&same); err != nil {
		return err
	}
	if !same {
		return domain.ErrConflict
	}
	for _, sha := range []string{o.Before, o.After} {
		if sha == "" {
			continue
		}
		j := domain.NewGitScan(o.RepoID, o.GitOrigin, sha, now)
		b, err = json.Marshal(j)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO git_scan_jobs(repo_id,id,payload,state,next_attempt,lease_until) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, j.RepoID, j.ID, b, j.State, j.NextAttempt, j.LeaseUntil)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresStore) ClaimGitHeadScan(ctx context.Context, repo domain.ContentHash, origin string, now time.Time, lease time.Duration) (domain.GitHeadScan, error) {
	j := domain.GitHeadScan{RepoID: repo, GitOrigin: origin, Page: 1, State: "waiting"}
	if err := j.Validate(); err != nil {
		return j, err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return j, err
	}
	defer tx.Rollback(ctx)
	b, _ := json.Marshal(j)
	if _, err = tx.Exec(ctx, `INSERT INTO git_head_scans(repo_id,origin,payload) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, repo, origin, b); err != nil {
		return j, err
	}
	if err = tx.QueryRow(ctx, `SELECT payload FROM git_head_scans WHERE repo_id=$1 AND origin=$2 FOR UPDATE`, repo, origin).Scan(&b); err != nil {
		return j, mapNoRows(err)
	}
	if err = json.Unmarshal(b, &j); err != nil {
		return j, err
	}
	j, err = j.Claim(now, lease)
	if err != nil {
		return j, err
	}
	b, _ = json.Marshal(j)
	if _, err = tx.Exec(ctx, `UPDATE git_head_scans SET payload=$3 WHERE repo_id=$1 AND origin=$2`, repo, origin, b); err != nil {
		return j, err
	}
	return j, tx.Commit(ctx)
}
func (s *PostgresStore) FinishGitHeadScan(ctx context.Context, j domain.GitHeadScan, observations []domain.GitRefObservation, more bool, now time.Time) error {
	if err := j.Validate(); err != nil {
		return err
	}
	if len(observations) > 100 {
		return domain.ErrValidation
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var b []byte
	var old domain.GitHeadScan
	if err = tx.QueryRow(ctx, `SELECT payload FROM git_head_scans WHERE repo_id=$1 AND origin=$2 FOR UPDATE`, j.RepoID, j.GitOrigin).Scan(&b); err != nil {
		return mapNoRows(err)
	}
	if err = json.Unmarshal(b, &old); err != nil {
		return err
	}
	if old.Version != j.Version || old.Page != j.Page || old.LeaseUntil.IsZero() {
		return domain.ErrConflict
	}
	for _, o := range observations {
		if o.RepoID != j.RepoID || o.GitOrigin != j.GitOrigin || o.Source != "reconciliation" {
			return domain.ErrIntegrity
		}
		if err = o.Validate(); err != nil {
			return err
		}
		if err = putGitObservationPG(ctx, tx, o, now); err != nil {
			return err
		}
	}
	next := j.Finish(more, now)
	b, _ = json.Marshal(next)
	if _, err = tx.Exec(ctx, `UPDATE git_head_scans SET payload=$3 WHERE repo_id=$1 AND origin=$2`, j.RepoID, j.GitOrigin, b); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var _ outbound.GitHeadScanStore = (*PostgresStore)(nil)

func (s *PostgresStore) GetGitHeadScan(ctx context.Context, repo domain.ContentHash, origin string) (domain.GitHeadScan, error) {
	var b []byte
	var j domain.GitHeadScan
	err := s.db(ctx).QueryRow(ctx, `SELECT payload FROM git_head_scans WHERE repo_id=$1 AND origin=$2`, repo, origin).Scan(&b)
	if err != nil {
		return j, mapNoRows(err)
	}
	if err = json.Unmarshal(b, &j); err != nil {
		return j, err
	}
	if j.RepoID != repo || j.GitOrigin != origin {
		return j, domain.ErrIntegrity
	}
	return j, j.Validate()
}
func (s *PostgresStore) FailGitHeadScan(ctx context.Context, j domain.GitHeadScan, now time.Time) error {
	if err := j.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(j.Defer(now))
	if err != nil {
		return err
	}
	tag, err := s.db(ctx).Exec(ctx, `UPDATE git_head_scans SET payload=$3 WHERE repo_id=$1 AND origin=$2 AND (payload->>'version')::bigint=$4 AND (payload->>'page')::int=$5 AND payload->>'state'='running'`, j.RepoID, j.GitOrigin, b, j.Version, j.Page)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}
