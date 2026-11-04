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

var _ outbound.DocJobStore = (*PostgresStore)(nil)

func (s *PostgresStore) GetDocJob(ctx context.Context, repo domain.ContentHash, id string) (j domain.DocFinalizationJob, err error) {
	var raw []byte
	var epoch string
	err = s.db(ctx).QueryRow(ctx, `SELECT job.payload,catalog.epoch::text FROM doc_finalization_jobs job
 JOIN repository_catalog_state catalog ON catalog.repo_id=job.repo_id WHERE job.repo_id=$1 AND job.id=$2`, repo, id).Scan(&raw, &epoch)
	if err != nil {
		return j, mapNoRows(err)
	}
	j, err = decodeDocJobLifetimePG(raw, epoch)
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
		if j.DocIdentity == domain.DocumentIdentityRootV1 {
			chunks, err := publicationJobChunks(j)
			if err != nil {
				return err
			}
			for _, h := range chunks {
				var one int
				if err := s.db(ctx).QueryRow(ctx, `SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2 FOR SHARE`, j.RepoID, h).Scan(&one); err != nil {
					return mapNoRows(err)
				}
			}
		}
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
				if j.DocIdentity == domain.DocumentIdentityRootV1 {
					proof, err := s.VerifyStoredDoc(ctx, j.RepoID, j.DocHash)
					if err != nil {
						return err
					}
					if proof.DocumentRef() != j.DocumentRef() {
						return domain.ErrIntegrity
					}
				}
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
		j.RepositoryEpoch, err = retainDocJobRepositoryPG(ctx, s.db(ctx), j.RepoID)
		if err != nil {
			return err
		}
		out = j
		return s.writeDocJob(ctx, j)
	})
	return
}

const claimDocJobCandidateSQL = `SELECT j.payload FROM doc_finalization_jobs j
 WHERE ($2='' OR j.repo_id=$2) AND j.state IN ('waiting','retrying','running') AND j.next_attempt<=$1 AND (j.state<>'running' OR j.lease_until<=$1)
 AND NOT EXISTS(SELECT 1 FROM doc_finalization_jobs p WHERE p.repo_id=j.repo_id AND p.id<>j.id AND p.state='running')
 AND (j.state='running' OR NOT EXISTS(SELECT 1 FROM doc_finalization_jobs p WHERE p.repo_id=j.repo_id AND p.state IN ('waiting','retrying') AND p.next_attempt<=$1 AND (p.created_at,p.id)<(j.created_at,j.id)))
 ORDER BY j.next_attempt,j.created_at,j.id LIMIT 1`

const claimDocJobSQL = claimDocJobCandidateSQL + ` FOR UPDATE OF j SKIP LOCKED`

func (s *PostgresStore) ClaimDocJob(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (j domain.DocFinalizationJob, err error) {
	tx, err := s.db(ctx).BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return j, err
	}
	defer rollbackPG(tx)
	var b []byte
	if err = tx.QueryRow(ctx, claimDocJobCandidateSQL, now, repo).Scan(&b); err != nil {
		return j, mapNoRows(err)
	}
	if err = json.Unmarshal(b, &j); err != nil {
		return j, err
	}
	epoch, err := retainDocJobRepositoryPG(ctx, tx, j.RepoID)
	if err != nil {
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
	if err = tx.QueryRow(ctx, claimDocJobSQL, now, j.RepoID).Scan(&b); err != nil {
		return j, mapNoRows(err)
	}
	if j, err = decodeDocJobLifetimePG(b, epoch); err != nil {
		return j, err
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
	old, err := readLockedDocJobPG(ctx, tx, j.RepoID, j.ID)
	if err != nil {
		return err
	}
	if !old.Fences(j, now) {
		return domain.ErrConflict
	}
	old.LeaseUntil = now.Add(lease)
	old.UpdatedAt = now
	b, err := json.Marshal(old)
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
	tx, err := s.db(ctx).BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	current, err := readLockedDocJobPG(ctx, tx, j.RepoID, j.ID)
	if err != nil {
		return err
	}
	if !current.Fences(j, now) {
		return domain.ErrConflict
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE doc_finalization_jobs SET payload=$3,state=$4,next_attempt=$5,lease_until=$6 WHERE repo_id=$1 AND id=$2`, j.RepoID, j.ID, b, j.State, j.NextAttempt, j.LeaseUntil); err != nil {
		return err
	}
	return storageWriteError(tx.Commit(ctx))
}
func (s *PostgresStore) CompleteDocJob(ctx context.Context, j domain.DocFinalizationJob, doc domain.VerifiedSessionDoc, _ time.Time) error {
	publication, err := s.PrepareDocJob(ctx, doc)
	if err != nil {
		return err
	}
	return publication.Complete(ctx, j, time.Now().UTC())
}

type pgDocPublication struct {
	store *PostgresStore
	doc   preparedDocPG
}

func (s *PostgresStore) PrepareDocJob(ctx context.Context, doc domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error) {
	var prepared preparedDocPG
	var err error
	if doc.DocumentRef().Identity == domain.DocumentIdentityRootV1 {
		prepared, err = prepareRootDocPG(ctx, doc)
	} else {
		prepared, err = prepareVerifiedDocPG(ctx, doc)
	}
	if err != nil {
		return nil, err
	}
	return pgDocPublication{s, prepared}, nil
}

func (p pgDocPublication) Complete(ctx context.Context, j domain.DocFinalizationJob, _ time.Time) error {
	s, doc := p.store, p.doc.doc
	// Prepare expensive GIN/search derivatives before the repository writer lock.
	// This grants no document access; the final transaction rechecks everything.
	if err := p.stageReadBlocks(ctx, j); err != nil {
		return err
	}
	return s.WithinRepository(ctx, j.RepoID, func(ctx context.Context) error {
		old, err := readLockedDocJobPG(ctx, s.db(ctx), j.RepoID, j.ID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if !old.Fences(j, now) {
			return domain.ErrConflict
		}
		if err := publicationJobMatches(old, doc); err != nil {
			return err
		}
		// WithinRepository retains the current repository policy row through
		// commit, including a requirement changed after proof preparation.
		repo, err := s.GetRepo(ctx, j.RepoID)
		if err != nil {
			return err
		}
		if err := checkDocJobPublicationPolicy(ctx, repo, doc.DocumentRef().Identity); err != nil {
			return err
		}
		if doc.DocumentRef().Identity == domain.DocumentIdentityRootV1 {
			if err := s.putRootDocJob(ctx, j.RepoID, p.doc); err != nil {
				return err
			}
		} else if _, err := s.putPreparedDoc(ctx, j.RepoID, p.doc); err != nil {
			return err
		}
		now = time.Now().UTC()
		if !old.Fences(j, now) {
			return domain.ErrConflict
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		old.State = "completed"
		old.Reason = ""
		old.UpdatedAt = now
		old.LeaseUntil = time.Time{}
		return s.writeDocJob(ctx, old)
	})
}
