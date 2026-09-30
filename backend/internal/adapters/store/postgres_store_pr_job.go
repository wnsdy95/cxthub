//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) EnqueuePRJob(ctx context.Context, j domain.PRPromotionJob) (domain.PRPromotionJob, error) {
	j = domain.AppendPRJobDiagnostic(domain.PRPromotionJob{}, j, "queued", j.CreatedAt)
	if err := j.Validate(); err != nil {
		return j, err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return j, err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO pr_promotion_jobs(repo_id,id,payload,state,created_at,next_attempt,lease_until,version) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, j.RepoID, j.ID, b, j.State, j.CreatedAt, j.NextAttempt, j.LeaseUntil, j.Version)
	if err != nil {
		return j, err
	}
	var raw []byte
	err = s.db(ctx).QueryRow(ctx, `SELECT payload FROM pr_promotion_jobs WHERE repo_id=$1 AND id=$2`, j.RepoID, j.ID).Scan(&raw)
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
	rows, err := s.db(ctx).Query(ctx, `SELECT payload FROM pr_promotion_jobs WHERE repo_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, repo)
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

const claimPRJobSQL = `SELECT j.payload FROM pr_promotion_jobs j
 WHERE ($1='' OR j.repo_id=$1) AND ($2='' OR j.id=$2)
 AND j.state IN ('waiting','retrying','running') AND j.next_attempt<=$3
 AND (j.state<>'running' OR j.lease_until<=$3)
 AND NOT EXISTS(SELECT 1 FROM pr_promotion_jobs p WHERE p.repo_id=j.repo_id AND p.id<>j.id AND p.state='running')
 AND (j.state='running' OR NOT EXISTS(SELECT 1 FROM pr_promotion_jobs p WHERE p.repo_id=j.repo_id AND p.state IN ('waiting','retrying','running') AND p.next_attempt<=$3 AND (p.created_at,p.id)<(j.created_at,j.id)))
 ORDER BY j.created_at,j.id FOR UPDATE OF j SKIP LOCKED LIMIT 1`

func (s *PostgresStore) ClaimPRJob(ctx context.Context, repo domain.ContentHash, id string, now time.Time, lease time.Duration) (domain.PRPromotionJob, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return domain.PRPromotionJob{}, err
	}
	defer tx.Rollback(ctx)
	var b []byte
	err = tx.QueryRow(ctx, claimPRJobSQL, repo, id, now).Scan(&b)
	if err != nil {
		return domain.PRPromotionJob{}, mapNoRows(err)
	}
	var j domain.PRPromotionJob
	if err = json.Unmarshal(b, &j); err != nil {
		return j, err
	}
	// Wake/retry can make an older row eligible while another claimant selected
	// a newer row. Serialize claims by repository, then recheck using a fresh
	// READ COMMITTED snapshot. Locking just the selected job cannot fence that race.
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "cxthub-pr-claim:"+string(j.RepoID)).Scan(&locked); err != nil {
		return j, err
	}
	if !locked {
		return domain.PRPromotionJob{}, domain.ErrNotFound
	}
	if err = tx.QueryRow(ctx, claimPRJobSQL, j.RepoID, j.ID, now).Scan(&b); err != nil {
		return domain.PRPromotionJob{}, mapNoRows(err)
	}
	if err = json.Unmarshal(b, &j); err != nil {
		return j, err
	}
	old := j
	j.State = "running"
	j.FailureClass = ""
	j.Attempts++
	j.Version++
	j.UpdatedAt = now
	j.LeaseUntil = now.Add(lease)
	j = domain.AppendPRJobDiagnostic(old, j, "claimed", now)
	if err = j.Validate(); err != nil {
		return j, err
	}
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
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var b []byte
	var version int64
	var state string
	if err = tx.QueryRow(ctx, `SELECT payload,version,state FROM pr_promotion_jobs WHERE repo_id=$1 AND id=$2 FOR UPDATE`, j.RepoID, j.ID).Scan(&b, &version, &state); err != nil {
		return mapNoRows(err)
	}
	var current domain.PRPromotionJob
	if err = json.Unmarshal(b, &current); err != nil {
		return err
	}
	if version != j.Version || state != "running" {
		return domain.ErrConflict
	}
	next, err := finishPRJob(current, j)
	if err != nil {
		return err
	}
	b, err = json.Marshal(next)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE pr_promotion_jobs SET payload=$3,state=$4,next_attempt=$5,lease_until=$6 WHERE repo_id=$1 AND id=$2 AND version=$7 AND state='running'`, next.RepoID, next.ID, b, next.State, next.NextAttempt, next.LeaseUntil, next.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return tx.Commit(ctx)
}
func (s *PostgresStore) RetryPRJob(ctx context.Context, repo domain.ContentHash, id string, now time.Time) error {
	tx, err := s.db(ctx).Begin(ctx)
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
	if retryPRJobUnchanged(j, now) {
		return nil
	}
	if j.State == "running" && j.LeaseUntil.After(now) {
		return domain.ErrConflict
	}
	old := j
	j.State = "waiting"
	j.NextAttempt = now
	j.UpdatedAt = now
	j.Version++
	j.Attempts = 0
	j.Reason = ""
	j.FailureClass = ""
	j.LeaseUntil = time.Time{}
	j = domain.AppendPRJobDiagnostic(old, j, "retry_requested", now)
	if err = j.Validate(); err != nil {
		return err
	}
	b, err = json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE pr_promotion_jobs SET payload=$3,state=$4,next_attempt=$5,version=$6,lease_until=$7 WHERE repo_id=$1 AND id=$2`, repo, id, b, j.State, j.NextAttempt, j.Version, j.LeaseUntil)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) GetPRJob(ctx context.Context, repo domain.ContentHash, id string) (domain.PRPromotionJob, error) {
	var j domain.PRPromotionJob
	var b []byte
	err := s.db(ctx).QueryRow(ctx, `SELECT payload FROM pr_promotion_jobs WHERE repo_id=$1 AND id=$2`, repo, id).Scan(&b)
	if err != nil {
		return j, mapNoRows(err)
	}
	err = json.Unmarshal(b, &j)
	return j, err
}

func (s *PostgresStore) WakePRSourceJobs(ctx context.Context, repo domain.ContentHash, now time.Time) error {
	// Filter by durable exact revisions, not the UI's latest-100 list. The Go
	// predicate below also checks ordinary alias proof and its attachment.
	rows, err := s.db(ctx).Query(ctx, `SELECT j.payload FROM pr_promotion_jobs j
 WHERE ($1='' OR j.repo_id=$1) AND j.state='attention' AND j.payload->>'reason'='source_finalization_required'
 AND EXISTS(SELECT 1 FROM context_history h WHERE h.repo_id=j.repo_id AND h.event->>'kind'='publish'
   AND h.event->>'git_after'=j.payload->'pr'->>'head_sha'
   AND (h.event->>'branch'=j.payload->'pr'->>'head_branch' OR h.event->>'local_branch'=j.payload->'pr'->>'head_branch'))`, repo)
	if err != nil {
		return err
	}
	var jobs []domain.PRPromotionJob
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var j domain.PRPromotionJob
		if err := json.Unmarshal(raw, &j); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	history := map[domain.ContentHash][]domain.HistoryEvent{}
	for _, j := range jobs {
		events, ok := history[j.RepoID]
		if !ok {
			events, err = s.ListHistoryEvents(ctx, j.RepoID)
			if err != nil {
				return err
			}
			history[j.RepoID] = events
		}
		if !hasPRSourcePublication(j, events) {
			continue
		}
		version := j.Version
		old := j
		j.State, j.Reason, j.Attempts, j.FailureClass = "waiting", "", 0, ""
		j.NextAttempt, j.UpdatedAt, j.LeaseUntil = now, now, time.Time{}
		j.Version++
		j = domain.AppendPRJobDiagnostic(old, j, "source_available", now)
		if err := j.Validate(); err != nil {
			return err
		}
		raw, err := json.Marshal(j)
		if err != nil {
			return err
		}
		// A concurrent retry/claim/finish must never be replaced by this wake.
		if _, err := s.db(ctx).Exec(ctx, `UPDATE pr_promotion_jobs SET payload=$3,state=$4,next_attempt=$5,lease_until=$6,version=$7
 WHERE repo_id=$1 AND id=$2 AND version=$8 AND state='attention' AND payload->>'reason'='source_finalization_required'`,
			j.RepoID, j.ID, raw, j.State, j.NextAttempt, j.LeaseUntil, j.Version, version); err != nil {
			return err
		}
	}
	return nil
}
