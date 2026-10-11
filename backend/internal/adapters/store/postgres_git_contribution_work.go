//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitContributionWorkStore = (*PostgresStore)(nil)

const gitContributionWorkColumnsPG = `admission_id,version,state,next_attempt,lease_until,payload`

func scanGitContributionWorkPG(row pgx.Row, repo domain.ContentHash) (domain.GitContributionWork, error) {
	var work domain.GitContributionWork
	var id, state string
	var version int64
	var next, lease time.Time
	var raw []byte
	if err := row.Scan(&id, &version, &state, &next, &lease, &raw); err != nil {
		return work, mapNoRows(err)
	}
	if err := json.Unmarshal(raw, &work); err != nil {
		return domain.GitContributionWork{}, domain.ErrIntegrity
	}
	if work.RepoID != repo || work.AdmissionID != id || work.Version != version || work.State != state || !work.NextAttempt.Equal(next) || !work.LeaseUntil.Equal(lease) {
		return domain.GitContributionWork{}, domain.ErrIntegrity
	}
	if work.ValidateRecorded() != nil {
		return domain.GitContributionWork{}, domain.ErrIntegrity
	}
	return work, nil
}

func enqueueGitContributionWorkPG(ctx context.Context, transaction pgx.Tx, admission domain.GitContributionAdmission) error {
	work := domain.GitContributionWork{RepoID: admission.Destination.RepoID, AdmissionID: admission.ID, AfterReadVersion: admission.AfterReadVersion,
		ReadCursor: admission.AfterReadVersion, State: "waiting", NextAttempt: time.Now().UTC().Truncate(time.Microsecond)}
	raw, err := json.Marshal(work)
	if err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO git_contribution_work(repo_id,admission_id,version,state,next_attempt,lease_until,payload) VALUES($1,$2,0,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, work.RepoID, work.AdmissionID, work.State, work.NextAttempt, work.LeaseUntil, raw); err != nil {
		return err
	}
	stored, err := scanGitContributionWorkPG(transaction.QueryRow(ctx, `SELECT `+gitContributionWorkColumnsPG+` FROM git_contribution_work WHERE repo_id=$1 AND admission_id=$2`, work.RepoID, work.AdmissionID), work.RepoID)
	if err != nil {
		return err
	}
	if stored.AdmissionID != work.AdmissionID || stored.AfterReadVersion != admission.AfterReadVersion {
		return domain.ErrIntegrity
	}
	return nil
}

func (s *PostgresStore) gitContributionWorkTransaction(ctx context.Context, repo domain.ContentHash) (*repositoryTx, error) {
	parent, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || parent.owner != s || parent.repo != repo || parent.readOnly || parent.Tx == nil || parent.Conn().PgConn().TxStatus() != 'T' {
		return nil, domain.ErrConflict
	}
	if err := s.checkRepositoryDocumentIdentity(ctx, parent, repo, true); err != nil {
		return nil, err
	}
	return parent, nil
}

func (s *PostgresStore) ClaimGitContributionWork(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.GitContributionWork, error) {
	var claimed domain.GitContributionWork
	if err := ctx.Err(); err != nil {
		return claimed, err
	}
	now = now.UTC().Truncate(time.Microsecond)
	until := now.Add(lease).Truncate(time.Microsecond)
	if domain.ValidateContentHash(repo) != nil || now.IsZero() || lease <= 0 || !until.After(now) {
		return claimed, domain.ErrValidation
	}
	err := s.WithinRepository(ctx, repo, func(bound context.Context) error {
		parent, err := s.gitContributionWorkTransaction(bound, repo)
		if err != nil {
			return err
		}
		transaction, err := parent.Begin(bound)
		if err != nil {
			return err
		}
		defer rollbackPG(transaction)
		work, err := scanGitContributionWorkPG(transaction.QueryRow(bound, `SELECT `+gitContributionWorkColumnsPG+` FROM git_contribution_work WHERE repo_id=$1 AND ((state IN ('waiting','retrying') AND next_attempt<=$2) OR (state='processing' AND lease_until<=$2)) ORDER BY next_attempt,admission_id LIMIT 1 FOR UPDATE`, repo, now), repo)
		if err != nil {
			return err
		}
		admission, err := readGitContributionAdmissionPG(bound, transaction, repo, work.AdmissionID)
		if err != nil {
			return err
		}
		if admission.AfterReadVersion != work.AfterReadVersion {
			return domain.ErrIntegrity
		}
		if work.Version == math.MaxInt64 || work.Attempts == math.MaxInt {
			return domain.ErrConflict
		}
		if work.ReadThrough == 0 {
			if err := transaction.QueryRow(bound, `SELECT GREATEST($4::bigint,COALESCE(MAX(read_version),0)) FROM git_context_heads WHERE repo_id=$1 AND origin=$2 AND destination_branch_id=$3`, repo, admission.Destination.GitOrigin, admission.Destination.DestinationBranchID, work.AfterReadVersion).Scan(&work.ReadThrough); err != nil {
				return err
			}
		}
		work.Version++
		work.Attempts++
		work.State, work.Reason, work.NextAttempt, work.LeaseUntil = "processing", "", now, until
		if work.Validate() != nil {
			return domain.ErrIntegrity
		}
		raw, err := json.Marshal(work)
		if err != nil {
			return err
		}
		if _, err := transaction.Exec(bound, `UPDATE git_contribution_work SET version=$3,state=$4,next_attempt=$5,lease_until=$6,payload=$7 WHERE repo_id=$1 AND admission_id=$2`, repo, work.AdmissionID, work.Version, work.State, work.NextAttempt, work.LeaseUntil, raw); err != nil {
			return err
		}
		if err := transaction.Commit(bound); err != nil {
			return err
		}
		claimed = work
		return nil
	})
	if err != nil {
		return domain.GitContributionWork{}, err
	}
	return claimed, nil
}

func (s *PostgresStore) FinishGitContributionWork(ctx context.Context, work domain.GitContributionWork) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if work.Validate() != nil || work.State == "processing" {
		return domain.ErrValidation
	}
	work.NextAttempt = work.NextAttempt.UTC().Truncate(time.Microsecond)
	parent, err := s.gitContributionWorkTransaction(ctx, work.RepoID)
	if err != nil {
		return err
	}
	transaction, err := parent.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	old, err := scanGitContributionWorkPG(transaction.QueryRow(ctx, `SELECT `+gitContributionWorkColumnsPG+` FROM git_contribution_work WHERE repo_id=$1 AND admission_id=$2 FOR UPDATE`, work.RepoID, work.AdmissionID), work.RepoID)
	if err != nil {
		return err
	}
	if old.State != "processing" || old.Version != work.Version || old.Attempts != work.Attempts || old.AfterReadVersion != work.AfterReadVersion {
		return domain.ErrConflict
	}
	admission, err := readGitContributionAdmissionPG(ctx, transaction, work.RepoID, work.AdmissionID)
	if err != nil {
		return err
	}
	if admission.AfterReadVersion != old.AfterReadVersion {
		return domain.ErrIntegrity
	}
	reset := work.ReadThrough == 0 && work.ReadCursor == work.AfterReadVersion && old.ReadCursor == old.ReadThrough
	if !reset && (work.ReadThrough != old.ReadThrough || work.ReadCursor < old.ReadCursor || work.ReadCursor > old.ReadThrough) {
		return domain.ErrConflict
	}
	if work.State == "completed" {
		if _, err := findGitContributionProofPG(ctx, transaction, work.RepoID, work.AdmissionID); errors.Is(err, domain.ErrNotFound) {
			return domain.ErrConflict
		} else if err != nil {
			return err
		}
	}
	raw, err := json.Marshal(work)
	if err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `UPDATE git_contribution_work SET state=$4,next_attempt=$5,lease_until=$6,payload=$7 WHERE repo_id=$1 AND admission_id=$2 AND version=$3 AND state='processing' AND lease_until>clock_timestamp()`, work.RepoID, work.AdmissionID, work.Version, work.State, work.NextAttempt, work.LeaseUntil, raw)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return transaction.Commit(ctx)
}
