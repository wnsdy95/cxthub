//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitAssociationStatusStore = (*PostgresStore)(nil)

func gitAssociationStatusQueryPG(request domain.GitAssociationStatusRequest) string {
	switch request.Kind {
	case "publication":
		query := `SELECT publication.publication_id FROM git_association_publications publication WHERE publication.repo_id=$1 AND publication.origin=$2 AND publication.publication_id>$3`
		if request.State == "pending" {
			query += ` AND NOT EXISTS (SELECT 1 FROM git_contribution_admissions admission WHERE admission.repo_id=publication.repo_id AND admission.origin=publication.origin AND admission.payload->>'publication_id'=publication.publication_id)`
		}
		return query + ` ORDER BY publication.publication_id LIMIT $4`
	case "integration":
		query := `SELECT work.admission_id FROM git_contribution_work work JOIN git_contribution_admissions admission ON admission.repo_id=work.repo_id AND admission.id=work.admission_id WHERE work.repo_id=$1 AND admission.origin=$2 AND work.admission_id>$3`
		if request.State == "pending" {
			query += ` AND NOT EXISTS (SELECT 1 FROM git_contribution_proofs proof WHERE proof.repo_id=work.repo_id AND proof.admission_id=work.admission_id)`
		}
		return query + ` ORDER BY work.admission_id LIMIT $4`
	case "memory":
		query := `SELECT supplement.id FROM git_contribution_supplements supplement WHERE supplement.repo_id=$1 AND supplement.origin=$2 AND supplement.id>$3`
		if request.State == "pending" {
			query += ` AND NOT EXISTS (SELECT 1 FROM git_contribution_memories memory WHERE memory.repo_id=supplement.repo_id AND memory.contribution_id=supplement.contribution_id AND memory.publication_id=supplement.publication_id)`
		}
		return query + ` ORDER BY supplement.id LIMIT $4`
	default:
		return ""
	}
}

func readGitStatusAdmissionPG(ctx context.Context, database catalogMerkleReader, repo domain.ContentHash, origin, id string) (domain.GitContributionAdmission, error) {
	admission, err := readGitContributionAdmissionPG(ctx, database, repo, id)
	if err != nil {
		return domain.GitContributionAdmission{}, gitStatusLinkErrorPG(err)
	}
	if admission.Destination.GitOrigin != origin {
		return domain.GitContributionAdmission{}, domain.ErrIntegrity
	}
	receipt, err := readGitHeadReceiptPG(ctx, database, repo, origin, admission.Destination.ReadVersion)
	if err != nil {
		return domain.GitContributionAdmission{}, gitStatusLinkErrorPG(err)
	}
	if !admission.MatchesBaseline(receipt) {
		return domain.GitContributionAdmission{}, domain.ErrIntegrity
	}
	return admission, nil
}

func gitStatusLinkErrorPG(err error) error {
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrIntegrity
	}
	return err
}

func readGitPublicationStatusPG(ctx context.Context, database pgDatabase, repo domain.ContentHash, origin, id string) (outbound.GitAssociationStatusEvidence, error) {
	var result outbound.GitAssociationStatusEvidence
	var raw []byte
	var accepted bool
	if err := database.QueryRow(ctx, `SELECT publication.payload,COALESCE(publication.payload=accepted.event,false) FROM git_association_publications publication LEFT JOIN context_history accepted ON accepted.repo_id=publication.repo_id AND accepted.id=publication.publication_id WHERE publication.repo_id=$1 AND publication.origin=$2 AND publication.publication_id=$3`, repo, origin, id).Scan(&raw, &accepted); err != nil {
		return result, gitStatusLinkErrorPG(mapNoRows(err))
	}
	publication, err := decodeGitAssociationPublicationPG(raw, repo, id)
	if err != nil {
		return result, err
	}
	if !accepted {
		return result, domain.ErrIntegrity
	}
	result.Publication = &publication
	var admissionID string
	err = database.QueryRow(ctx, `SELECT id FROM git_contribution_admissions WHERE repo_id=$1 AND origin=$2 AND payload->>'publication_id'=$3 ORDER BY id LIMIT 1`, repo, origin, id).Scan(&admissionID)
	if errors.Is(mapNoRows(err), domain.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	admission, err := readGitStatusAdmissionPG(ctx, database, repo, origin, admissionID)
	if err != nil {
		return result, err
	}
	name := publication.LocalBranch
	if name == "" {
		name = publication.Branch
	}
	if admission.PublicationID != publication.ID || admission.SourceBranchID != publication.BranchID || admission.SourceBranch != name || admission.Capture != publication.Target || admission.Capture != publication.Source || admission.OriginalHead != publication.GitAfter {
		return result, domain.ErrIntegrity
	}
	result.Admission = &admission
	return result, nil
}

func readGitIntegrationStatusPG(ctx context.Context, database pgDatabase, repo domain.ContentHash, origin, id string) (outbound.GitAssociationStatusEvidence, error) {
	var result outbound.GitAssociationStatusEvidence
	work, err := scanGitContributionWorkPG(database.QueryRow(ctx, `SELECT `+gitContributionWorkColumnsPG+` FROM git_contribution_work WHERE repo_id=$1 AND admission_id=$2`, repo, id), repo)
	if err != nil {
		return result, gitStatusLinkErrorPG(err)
	}
	admission, err := readGitStatusAdmissionPG(ctx, database, repo, origin, id)
	if err != nil {
		return result, err
	}
	if work.AdmissionID != admission.ID || work.AfterReadVersion != admission.AfterReadVersion {
		return result, domain.ErrIntegrity
	}
	result.Work, result.Admission = &work, &admission
	proof, err := findGitContributionProofPG(ctx, database, repo, admission.ID)
	if errors.Is(err, domain.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if proof.Contribution.GitOrigin != origin {
		return result, domain.ErrIntegrity
	}
	result.Proof = &proof
	return result, nil
}

func readGitMemoryStatusPG(ctx context.Context, database pgDatabase, repo domain.ContentHash, origin, id string) (outbound.GitAssociationStatusEvidence, error) {
	var result outbound.GitAssociationStatusEvidence
	job, err := scanGitSupplementPG(database.QueryRow(ctx, `SELECT `+gitSupplementColumnsPG+` FROM git_contribution_supplements WHERE repo_id=$1 AND origin=$2 AND id=$3`, repo, origin, id), repo)
	if err != nil {
		return result, gitStatusLinkErrorPG(err)
	}
	if job.ID != id || job.GitOrigin != origin {
		return result, domain.ErrIntegrity
	}
	proof, publication, err := readGitSupplementInputsPG(ctx, database, job)
	if err != nil {
		return result, gitStatusLinkErrorPG(err)
	}
	admission, err := readGitStatusAdmissionPG(ctx, database, repo, origin, proof.AdmissionID)
	if err != nil {
		return result, err
	}
	result.Supplement, result.Publication, result.Admission, result.Proof = &job, &publication, &admission, &proof
	memory, err := readGitSupplementMemoryPG(ctx, database, repo, job.ContributionID, job.PublicationID)
	if errors.Is(err, domain.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Memory = &memory
	return result, nil
}

func (s *PostgresStore) ReadGitAssociationStatus(ctx context.Context, repo domain.ContentHash, origin string, request domain.GitAssociationStatusRequest) ([]outbound.GitAssociationStatusEvidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Validate() != nil || validateGitHeadReceiptScope(repo, origin, 0) != nil {
		return nil, domain.ErrValidation
	}
	metadata, err := s.GetRepo(ctx, repo)
	if err != nil {
		return nil, err
	}
	if metadata.GitRemoteURL != origin {
		return nil, domain.ErrConflict
	}
	database := s.db(ctx)
	rows, err := database.Query(ctx, gitAssociationStatusQueryPG(request), repo, origin, request.Cursor, request.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0, request.Limit+1)
	previous := request.Cursor
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		checked := request
		checked.Cursor = id
		if id == "" || checked.Validate() != nil || id <= previous || len(ids) == request.Limit+1 {
			return nil, domain.ErrIntegrity
		}
		ids = append(ids, id)
		previous = id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	result := make([]outbound.GitAssociationStatusEvidence, 0, len(ids))
	for _, id := range ids {
		var evidence outbound.GitAssociationStatusEvidence
		switch request.Kind {
		case "publication":
			evidence, err = readGitPublicationStatusPG(ctx, database, repo, origin, id)
		case "integration":
			evidence, err = readGitIntegrationStatusPG(ctx, database, repo, origin, id)
		case "memory":
			evidence, err = readGitMemoryStatusPG(ctx, database, repo, origin, id)
		}
		if err != nil {
			return nil, err
		}
		if request.State == "pending" && (request.Kind == "publication" && evidence.Admission != nil || request.Kind == "integration" && evidence.Proof != nil || request.Kind == "memory" && evidence.Memory != nil) {
			return nil, domain.ErrIntegrity
		}
		result = append(result, evidence)
	}
	return result, nil
}
