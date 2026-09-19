//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.DocJobStore = (*PostgresStore)(nil)

func (s *PostgresStore) GetDocJob(ctx context.Context, repo domain.ContentHash, id string) (j domain.DocFinalizationJob, err error) {
	var raw []byte
	err = s.db(ctx).QueryRow(ctx, `SELECT payload FROM doc_finalization_jobs WHERE repo_id=$1 AND id=$2`, repo, id).Scan(&raw)
	if err != nil {
		return j, mapNoRows(err)
	}
	err = json.Unmarshal(raw, &j)
	if err == nil {
		err = j.Validate()
	}
	return
}
func (s *PostgresStore) writeDocJob(ctx context.Context, j domain.DocFinalizationJob) error {
	if err := j.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO doc_finalization_jobs(repo_id,id,payload,state,version,created_at,next_attempt,lease_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
 ON CONFLICT(repo_id,id) DO UPDATE SET payload=$3,state=$4,version=$5,next_attempt=$7,lease_until=$8`, j.RepoID, j.ID, b, j.State, j.Version, j.CreatedAt, j.NextAttempt, j.LeaseUntil)
	return err
}
func (s *PostgresStore) EnqueueDocJob(ctx context.Context, j domain.DocFinalizationJob) (out domain.DocFinalizationJob, err error) {
	if err = j.Validate(); err != nil {
		return j, err
	}
	if j.State != "waiting" || j.Version != 0 || j.Attempts != 0 {
		return j, domain.ErrValidation
	}
	err = s.WithinRepository(ctx, j.RepoID, func(ctx context.Context) error {
		old, e := s.GetDocJob(ctx, j.RepoID, j.ID)
		if e == nil {
			if old.State != "completed" {
				out = old
				return nil
			}
			have, e := s.HasDocs(ctx, j.RepoID, []domain.ContentHash{j.DocHash})
			if e != nil {
				return e
			}
			if len(have) > 0 {
				out = old
				return nil
			}
			j.Version = old.Version + 1
		} else if !errors.Is(e, domain.ErrNotFound) {
			return e
		}
		var count int
		if e = s.db(ctx).QueryRow(ctx, `SELECT count(*) FROM doc_finalization_jobs WHERE repo_id=$1 AND state IN ('waiting','running','retrying')`, j.RepoID).Scan(&count); e != nil {
			return e
		}
		if count >= domain.MaxPendingDocJobs {
			return domain.ErrConflict
		}
		out = j
		return s.writeDocJob(ctx, j)
	})
	return
}

const claimDocJobSQL = `SELECT j.payload FROM doc_finalization_jobs j
 WHERE ($2='' OR j.repo_id=$2) AND j.state IN ('waiting','retrying','running') AND j.next_attempt<=$1 AND (j.state<>'running' OR j.lease_until<=$1)
 AND NOT EXISTS(SELECT 1 FROM doc_finalization_jobs p WHERE p.repo_id=j.repo_id AND p.id<>j.id AND p.state='running')
 AND (j.state='running' OR NOT EXISTS(SELECT 1 FROM doc_finalization_jobs p WHERE p.repo_id=j.repo_id AND p.state IN ('waiting','retrying') AND p.next_attempt<=$1 AND (p.created_at,p.id)<(j.created_at,j.id)))
 ORDER BY j.next_attempt,j.created_at,j.id FOR UPDATE OF j SKIP LOCKED LIMIT 1`

func (s *PostgresStore) ClaimDocJob(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (j domain.DocFinalizationJob, err error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return j, err
	}
	defer rollbackPG(tx)
	var b []byte
	if err = tx.QueryRow(ctx, claimDocJobSQL, now, repo).Scan(&b); err != nil {
		return j, mapNoRows(err)
	}
	if err = json.Unmarshal(b, &j); err != nil {
		return j, err
	}
	// Row locks alone allow two different jobs for one repo to be selected under
	// READ COMMITTED. Serialize that repository's claim and recheck fresh state.
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "cxthub-doc-claim:"+string(j.RepoID)).Scan(&locked); err != nil {
		return j, err
	}
	if !locked {
		return j, domain.ErrNotFound
	}
	var other bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_finalization_jobs WHERE repo_id=$1 AND id<>$2 AND state='running')`, j.RepoID, j.ID).Scan(&other); err != nil {
		return j, err
	}
	if other {
		return j, domain.ErrNotFound
	}
	if err = j.Validate(); err != nil {
		return j, err
	}
	j = j.Claim(now, lease)
	b, err = json.Marshal(j)
	if err != nil {
		return j, err
	}
	_, err = tx.Exec(ctx, `UPDATE doc_finalization_jobs SET payload=$3,state='running',version=$4,lease_until=$5 WHERE repo_id=$1 AND id=$2`, j.RepoID, j.ID, b, j.Version, j.LeaseUntil)
	if err != nil {
		return j, err
	}
	return j, tx.Commit(ctx)
}
func (s *PostgresStore) RenewDocJob(ctx context.Context, j domain.DocFinalizationJob, now time.Time, lease time.Duration) error {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	var b []byte
	if err := tx.QueryRow(ctx, `SELECT payload FROM doc_finalization_jobs WHERE repo_id=$1 AND id=$2 FOR UPDATE`, j.RepoID, j.ID).Scan(&b); err != nil {
		return mapNoRows(err)
	}
	var old domain.DocFinalizationJob
	if err := json.Unmarshal(b, &old); err != nil {
		return err
	}
	if !old.Fences(j, now) {
		return domain.ErrConflict
	}
	old.LeaseUntil = now.Add(lease)
	old.UpdatedAt = now
	b, err = json.Marshal(old)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE doc_finalization_jobs SET payload=$3,lease_until=$4 WHERE repo_id=$1 AND id=$2`, j.RepoID, j.ID, b, old.LeaseUntil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *PostgresStore) FinishDocJob(ctx context.Context, j domain.DocFinalizationJob, now time.Time) error {
	if j.State != "retrying" && j.State != "rejected" {
		return domain.ErrValidation
	}
	if err := j.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	tag, err := s.db(ctx).Exec(ctx, `UPDATE doc_finalization_jobs SET payload=$4,state=$5,next_attempt=$6,lease_until=$7 WHERE repo_id=$1 AND id=$2 AND version=$3 AND state='running' AND lease_until>$8`, j.RepoID, j.ID, j.Version, b, j.State, j.NextAttempt, j.LeaseUntil, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}
func (s *PostgresStore) CompleteDocJob(ctx context.Context, j domain.DocFinalizationJob, doc domain.VerifiedSessionDoc, _ time.Time) error {
	return s.WithinRepository(ctx, j.RepoID, func(ctx context.Context) error {
		var raw []byte
		if err := s.db(ctx).QueryRow(ctx, `SELECT payload FROM doc_finalization_jobs WHERE repo_id=$1 AND id=$2 FOR UPDATE`, j.RepoID, j.ID).Scan(&raw); err != nil {
			return mapNoRows(err)
		}
		var old domain.DocFinalizationJob
		if err := json.Unmarshal(raw, &old); err != nil {
			return err
		}
		now := time.Now().UTC()
		if !old.Fences(j, now) {
			return domain.ErrConflict
		}
		if !doc.Valid() || doc.Hash() != old.DocHash {
			return domain.ErrIntegrity
		}
		if _, err := s.PutVerifiedDoc(ctx, j.RepoID, doc); err != nil {
			return err
		}
		old.State = "completed"
		old.Reason = ""
		old.UpdatedAt = now
		old.LeaseUntil = time.Time{}
		return s.writeDocJob(ctx, old)
	})
}
