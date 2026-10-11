//go:build postgres

package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitContributionSupplementStore = (*PostgresStore)(nil)

const gitSupplementColumnsPG = `repo_id,id,origin,contribution_id,publication_id,proof_id,version,state,next_attempt,lease_until,payload`
const enqueueGitSupplementsPG = `INSERT INTO git_contribution_supplements(` + gitSupplementColumnsPG + `) SELECT ` + gitSupplementColumnsPG + ` FROM git_contribution_supplement_candidates `

func enqueueGitProofSupplementsPG(ctx context.Context, transaction pgx.Tx, proof domain.GitContributionProof) error {
	repo, contributionID := proof.Contribution.RepoID, proof.Contribution.ID
	if _, err := transaction.Exec(ctx, `INSERT INTO git_contribution_supplement_anchors(repo_id,contribution_id,proof_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, repo, contributionID, proof.ID); err != nil {
		return err
	}
	var proofID string
	if err := transaction.QueryRow(ctx, `SELECT proof_id FROM git_contribution_supplement_anchors WHERE repo_id=$1 AND contribution_id=$2`, repo, contributionID).Scan(&proofID); err != nil {
		return mapNoRows(err)
	}
	accepted, err := readGitSupplementProofPG(ctx, transaction, repo, proofID)
	if err != nil {
		return err
	}
	if accepted.Contribution != proof.Contribution {
		return domain.ErrIntegrity
	}
	_, err = transaction.Exec(ctx, enqueueGitSupplementsPG+`WHERE repo_id=$1 AND contribution_id=$2 ON CONFLICT DO NOTHING`, repo, contributionID)
	return err
}

func enqueueGitPublicationSupplementsPG(ctx context.Context, transaction pgx.Tx, repo domain.ContentHash, origin string, publication domain.HistoryEvent) error {
	if !publication.MemoryPinned {
		return nil
	}
	_, err := transaction.Exec(ctx, enqueueGitSupplementsPG+`WHERE repo_id=$1 AND origin=$2 AND publication_id=$3 ON CONFLICT DO NOTHING`, repo, origin, publication.ID)
	return err
}

func scanGitSupplementPG(row pgx.Row, repo domain.ContentHash) (domain.GitContributionSupplement, error) {
	var storedRepo domain.ContentHash
	var id, origin, contributionID, publicationID, proofID, state string
	var version int64
	var next, lease time.Time
	var raw []byte
	if err := row.Scan(&storedRepo, &id, &origin, &contributionID, &publicationID, &proofID, &version, &state, &next, &lease, &raw); err != nil {
		return domain.GitContributionSupplement{}, mapNoRows(err)
	}
	job, err := decodePRHistory[domain.GitContributionSupplement](raw)
	if err != nil {
		return domain.GitContributionSupplement{}, err
	}
	if storedRepo != repo || job.RepoID != repo || job.ID != id || job.GitOrigin != origin || job.ContributionID != contributionID || job.PublicationID != publicationID || job.ProofID != proofID || job.Version != version || job.State != state || !job.NextAttempt.Equal(next) || !job.LeaseUntil.Equal(lease) {
		return domain.GitContributionSupplement{}, domain.ErrIntegrity
	}
	return job, nil
}

func readGitSupplementProofPG(ctx context.Context, database catalogMerkleReader, repo domain.ContentHash, id string) (domain.GitContributionProof, error) {
	proof, err := scanGitContributionProofPG(database.QueryRow(ctx, gitContributionProofQueryPG+` WHERE proof.repo_id=$1 AND proof.id=$2`, repo, id), repo)
	if err == nil && proof.ID != id {
		return domain.GitContributionProof{}, domain.ErrIntegrity
	}
	return proof, err
}

func (s *PostgresStore) GetGitContributionSupplementProof(ctx context.Context, repo domain.ContentHash, id string) (domain.GitContributionProof, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitContributionProof{}, err
	}
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(domain.ContentHash("sha256:"+id)) != nil {
		return domain.GitContributionProof{}, domain.ErrValidation
	}
	return readGitSupplementProofPG(ctx, s.db(ctx), repo, id)
}

func readGitSupplementInputsPG(ctx context.Context, database catalogMerkleReader, job domain.GitContributionSupplement) (domain.GitContributionProof, domain.HistoryEvent, error) {
	proof, err := readGitSupplementProofPG(ctx, database, job.RepoID, job.ProofID)
	if err != nil {
		return proof, domain.HistoryEvent{}, err
	}
	record := proof.Contribution
	if record.ID != job.ContributionID || record.GitOrigin != job.GitOrigin {
		return proof, domain.HistoryEvent{}, domain.ErrIntegrity
	}
	var raw []byte
	var accepted bool
	if err := database.QueryRow(ctx, `SELECT publication.payload,COALESCE(publication.payload=accepted.event,false) FROM git_association_publications publication LEFT JOIN context_history accepted ON accepted.repo_id=publication.repo_id AND accepted.id=publication.publication_id WHERE publication.repo_id=$1 AND publication.origin=$2 AND publication.publication_id=$3`, job.RepoID, job.GitOrigin, job.PublicationID).Scan(&raw, &accepted); err != nil {
		return proof, domain.HistoryEvent{}, mapNoRows(err)
	}
	publication, err := decodeGitAssociationPublicationPG(raw, job.RepoID, job.PublicationID)
	if err != nil {
		return proof, publication, err
	}
	if !accepted || !publication.MemoryPinned || publication.BranchID != record.SourceBranchID || publication.Source != record.Capture || publication.Target != record.Capture || publication.GitAfter != record.OriginalHead {
		return proof, publication, domain.ErrIntegrity
	}
	return proof, publication, nil
}

func readGitSupplementMemoryPG(ctx context.Context, database interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, repo domain.ContentHash, contributionID, publicationID string) (domain.GitContributionMemory, error) {
	rows, err := database.Query(ctx, `SELECT memory.id,memory.contribution_id,memory.publication_id,memory.memory_hash,memory.memory_source,memory.memory_pinned,memory.payload,contribution.payload FROM git_contribution_memories memory LEFT JOIN git_contributions contribution ON contribution.repo_id=memory.repo_id AND contribution.id=memory.contribution_id WHERE memory.repo_id=$1 AND memory.contribution_id=$2 AND memory.publication_id=$3 ORDER BY memory.id LIMIT 2`, repo, contributionID, publicationID)
	if err != nil {
		return domain.GitContributionMemory{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return domain.GitContributionMemory{}, err
		}
		return domain.GitContributionMemory{}, domain.ErrNotFound
	}
	var id, storedContribution, storedPublication string
	var hash, source domain.ContentHash
	var pinned bool
	var raw, contributionRaw []byte
	if err := rows.Scan(&id, &storedContribution, &storedPublication, &hash, &source, &pinned, &raw, &contributionRaw); err != nil {
		return domain.GitContributionMemory{}, err
	}
	if rows.Next() {
		return domain.GitContributionMemory{}, domain.ErrIntegrity
	}
	if err := rows.Err(); err != nil {
		return domain.GitContributionMemory{}, err
	}
	memory, err := decodePRHistory[domain.GitContributionMemory](raw)
	if err != nil {
		return domain.GitContributionMemory{}, err
	}
	if storedContribution != contributionID || storedPublication != publicationID || memory.ID != id || memory.RepoID != repo || memory.ContributionID != contributionID || memory.PublicationID != publicationID || memory.MemoryHash != hash || memory.MemorySource != source || memory.MemoryPinned != pinned {
		return domain.GitContributionMemory{}, domain.ErrIntegrity
	}
	contribution, err := decodePRHistory[domain.GitContribution](contributionRaw)
	if err != nil || contribution.RepoID != repo || contribution.ID != contributionID {
		return domain.GitContributionMemory{}, domain.ErrIntegrity
	}
	return memory, nil
}

func (s *PostgresStore) FindGitContributionSupplementMemory(ctx context.Context, repo domain.ContentHash, contributionID, publicationID string) (domain.GitContributionMemory, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitContributionMemory{}, err
	}
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(domain.ContentHash("sha256:"+contributionID)) != nil || len(publicationID) != 32 || publicationID != strings.ToLower(publicationID) {
		return domain.GitContributionMemory{}, domain.ErrValidation
	}
	if _, err := hex.DecodeString(publicationID); err != nil {
		return domain.GitContributionMemory{}, domain.ErrValidation
	}
	return readGitSupplementMemoryPG(ctx, s.db(ctx), repo, contributionID, publicationID)
}

func (s *PostgresStore) ClaimGitContributionSupplement(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.GitContributionSupplement, error) {
	var claimed domain.GitContributionSupplement
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
		job, err := scanGitSupplementPG(transaction.QueryRow(bound, `SELECT `+gitSupplementColumnsPG+` FROM git_contribution_supplements WHERE repo_id=$1 AND ((state IN ('waiting','retrying') AND next_attempt<=$2) OR (state='processing' AND lease_until<=$2)) ORDER BY next_attempt,id LIMIT 1 FOR UPDATE`, repo, now), repo)
		if err != nil {
			return err
		}
		if job.Version == math.MaxInt64 || job.Attempts == math.MaxInt {
			return domain.ErrConflict
		}
		job.Version++
		job.Attempts++
		job.State, job.Reason, job.NextAttempt, job.LeaseUntil = "processing", "", now, until
		if job.Validate() != nil {
			return domain.ErrIntegrity
		}
		raw, err := json.Marshal(job)
		if err != nil {
			return err
		}
		if _, err := transaction.Exec(bound, `UPDATE git_contribution_supplements SET version=$3,state=$4,next_attempt=$5,lease_until=$6,payload=$7 WHERE repo_id=$1 AND id=$2`, repo, job.ID, job.Version, job.State, job.NextAttempt, job.LeaseUntil, raw); err != nil {
			return err
		}
		if err := transaction.Commit(bound); err != nil {
			return err
		}
		claimed = job
		return nil
	})
	if err != nil {
		return domain.GitContributionSupplement{}, err
	}
	return claimed, nil
}

func (s *PostgresStore) FinishGitContributionSupplement(ctx context.Context, job domain.GitContributionSupplement) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if job.Validate() != nil || job.State == "processing" || job.Version == 0 {
		return domain.ErrValidation
	}
	job.NextAttempt = job.NextAttempt.UTC().Truncate(time.Microsecond)
	parent, err := s.gitContributionWorkTransaction(ctx, job.RepoID)
	if err != nil {
		return err
	}
	transaction, err := parent.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	old, err := scanGitSupplementPG(transaction.QueryRow(ctx, `SELECT `+gitSupplementColumnsPG+` FROM git_contribution_supplements WHERE repo_id=$1 AND id=$2 FOR UPDATE`, job.RepoID, job.ID), job.RepoID)
	if err != nil {
		return err
	}
	if old.State != "processing" || old.Version != job.Version || old.Attempts != job.Attempts || old.GitOrigin != job.GitOrigin || old.ProofID != job.ProofID || old.ContributionID != job.ContributionID || old.PublicationID != job.PublicationID {
		return domain.ErrConflict
	}
	if job.State == "completed" {
		proof, publication, err := readGitSupplementInputsPG(ctx, transaction, job)
		if err != nil {
			return err
		}
		memory, err := readGitSupplementMemoryPG(ctx, transaction, job.RepoID, job.ContributionID, job.PublicationID)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrConflict
		}
		if err != nil {
			return err
		}
		source := publication.MemorySource
		if source == "" && publication.MemoryHash != "" {
			source = proof.Contribution.Capture
		}
		want := (domain.GitContributionMemory{RepoID: job.RepoID, ContributionID: job.ContributionID, PublicationID: job.PublicationID, MemoryHash: publication.MemoryHash, MemorySource: source, MemoryPinned: true}).WithID()
		if memory != want {
			return domain.ErrIntegrity
		}
	}
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `UPDATE git_contribution_supplements SET state=$4,next_attempt=$5,lease_until=$6,payload=$7 WHERE repo_id=$1 AND id=$2 AND version=$3 AND state='processing' AND lease_until>clock_timestamp()`, job.RepoID, job.ID, job.Version, job.State, job.NextAttempt, job.LeaseUntil, raw)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return transaction.Commit(ctx)
}
