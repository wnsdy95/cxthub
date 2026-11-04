//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitContextHeadStore = (*PostgresStore)(nil)

const gitContextHeadColumnsPG = `head.id,head.origin,head.destination_branch_id,head.destination_branch,head.read_version,head.receipt_id,head.payload,COALESCE(receipt.origin,''),COALESCE(receipt.version,0),receipt.payload`
const gitContextHeadJoinPG = ` FROM git_context_heads head LEFT JOIN git_head_receipts receipt ON receipt.repo_id=head.repo_id AND receipt.id=head.receipt_id`

type gitContextHeadRowPG struct {
	id, origin, branchID, branch, receiptID string
	version                                 int64
	raw                                     []byte
	receiptOrigin                           string
	receiptVersion                          int64
	receiptRaw                              []byte
}

func (row *gitContextHeadRowPG) columns() []any {
	return []any{&row.id, &row.origin, &row.branchID, &row.branch, &row.version, &row.receiptID, &row.raw, &row.receiptOrigin, &row.receiptVersion, &row.receiptRaw}
}

func (row gitContextHeadRowPG) decode(repo domain.ContentHash) (domain.GitContextHead, domain.GitHeadReceipt, error) {
	head, err := decodePRHistory[domain.GitContextHead](row.raw)
	if err != nil {
		return head, domain.GitHeadReceipt{}, err
	}
	if err := validateGitContextHeadIdentity(head, repo, row.id, row.origin, row.branchID, row.branch, row.version, row.receiptID); err != nil {
		return domain.GitContextHead{}, domain.GitHeadReceipt{}, err
	}
	receipt, err := decodePRHistory[domain.GitHeadReceipt](row.receiptRaw)
	if err != nil || validateGitContextHeadReceipt(head, receipt, repo, row.receiptOrigin, row.receiptVersion, row.receiptID) != nil {
		return domain.GitContextHead{}, domain.GitHeadReceipt{}, domain.ErrIntegrity
	}
	return head, receipt, nil
}

func readGitContextHeadPG(ctx context.Context, database catalogMerkleReader, repo domain.ContentHash, id string) (domain.GitContextHead, domain.GitHeadReceipt, error) {
	var row gitContextHeadRowPG
	if err := database.QueryRow(ctx, `SELECT `+gitContextHeadColumnsPG+gitContextHeadJoinPG+` WHERE head.repo_id=$1 AND head.id=$2`, repo, id).Scan(row.columns()...); err != nil {
		return domain.GitContextHead{}, domain.GitHeadReceipt{}, mapNoRows(err)
	}
	head, receipt, err := row.decode(repo)
	if err == nil && head.ID != id {
		err = domain.ErrIntegrity
	}
	return head, receipt, err
}

func (s *PostgresStore) gitContextWriteTransaction(ctx context.Context, repo domain.ContentHash, origin string) (*repositoryTx, error) {
	parent, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || parent.owner != s || parent.repo != repo || parent.readOnly || parent.Tx == nil || parent.Conn().PgConn().TxStatus() != 'T' {
		return nil, domain.ErrConflict
	}
	if err := s.checkRepositoryDocumentIdentity(ctx, parent, repo, true); err != nil {
		return nil, err
	}
	metadata, err := s.GetRepo(ctx, repo)
	if err != nil {
		return nil, err
	}
	if metadata.GitRemoteURL != origin {
		return nil, domain.ErrConflict
	}
	return parent, nil
}

func (s *PostgresStore) PutGitContextHeads(ctx context.Context, receipt domain.GitHeadReceipt, heads []domain.GitContextHead) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if receipt.Validate() != nil || len(heads) > 100 {
		return domain.ErrValidation
	}
	branches, names := map[string]bool{}, map[string]bool{}
	for _, head := range heads {
		if head.ID != "" && head.ID != head.WithID().ID {
			return domain.ErrIntegrity
		}
		if err := head.Validate(); err != nil {
			return err
		}
		destination := head.Destination
		if !destination.MatchesReceipt(receipt) {
			return domain.ErrIntegrity
		}
		if branches[destination.DestinationBranchID] || names[destination.DestinationBranch] {
			return domain.ErrValidation
		}
		branches[destination.DestinationBranchID], names[destination.DestinationBranch] = true, true
	}
	parent, err := s.gitContextWriteTransaction(ctx, receipt.RepoID, receipt.GitOrigin)
	if err != nil {
		return err
	}
	transaction, err := parent.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if err := validateGitAdmissionMatchReceiptPG(ctx, transaction, receipt); err != nil {
		return err
	}
	for _, head := range heads {
		raw, err := json.Marshal(head)
		if err != nil {
			return err
		}
		destination := head.Destination
		if _, err := transaction.Exec(ctx, `INSERT INTO git_context_heads(repo_id,id,origin,destination_branch_id,destination_branch,read_version,receipt_id,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, receipt.RepoID, head.ID, receipt.GitOrigin, destination.DestinationBranchID, destination.DestinationBranch, receipt.Version, receipt.ID, raw); err != nil {
			return err
		}
		old, _, err := readGitContextHeadPG(ctx, transaction, receipt.RepoID, head.ID)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrConflict
		}
		if err != nil {
			return err
		}
		if old != head {
			return domain.ErrConflict
		}
	}
	return transaction.Commit(ctx)
}

func (s *PostgresStore) GetGitContextHead(ctx context.Context, repo domain.ContentHash, id string) (domain.GitContextHead, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitContextHead{}, err
	}
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(domain.ContentHash("sha256:"+id)) != nil {
		return domain.GitContextHead{}, domain.ErrValidation
	}
	head, _, err := readGitContextHeadPG(ctx, s.db(ctx), repo, id)
	return head, err
}

func (s *PostgresStore) ListGitContextHeads(ctx context.Context, repo domain.ContentHash, origin, branchID string, afterVersion int64, limit int) ([]domain.GitContextHead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if validateGitHeadReceiptScope(repo, origin, afterVersion) != nil || branchID == "" || len(branchID) > 128 || strings.TrimSpace(branchID) != branchID || strings.ContainsAny(branchID, "\x00\r\n") || limit < 1 || limit > 101 {
		return nil, domain.ErrValidation
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT `+gitContextHeadColumnsPG+gitContextHeadJoinPG+` WHERE head.repo_id=$1 AND head.origin=$2 AND head.destination_branch_id=$3 AND head.read_version>$4 ORDER BY head.read_version,head.id LIMIT $5`, repo, origin, branchID, afterVersion, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.GitContextHead{}
	for rows.Next() {
		var row gitContextHeadRowPG
		if err := rows.Scan(row.columns()...); err != nil {
			return nil, err
		}
		head, _, err := row.decode(repo)
		if err != nil {
			return nil, err
		}
		if row.origin != origin || row.branchID != branchID || row.version <= afterVersion {
			return nil, domain.ErrIntegrity
		}
		result = append(result, head)
		afterVersion = row.version
	}
	return result, rows.Err()
}

const gitContributionProofQueryPG = `SELECT proof.id,proof.admission_id,proof.context_head_id,proof.contribution_id,proof.payload,
 COALESCE(admission.admission_key,''),COALESCE(admission.origin,''),COALESCE(admission.receipt_id,''),admission.payload,
 COALESCE(baseline.origin,''),COALESCE(baseline.version,0),baseline.payload,contribution.payload,` + gitContextHeadColumnsPG + `
 FROM git_contribution_proofs proof
 LEFT JOIN git_contribution_admissions admission ON admission.repo_id=proof.repo_id AND admission.id=proof.admission_id
 LEFT JOIN git_head_receipts baseline ON baseline.repo_id=admission.repo_id AND baseline.id=admission.receipt_id
 LEFT JOIN git_contributions contribution ON contribution.repo_id=proof.repo_id AND contribution.id=proof.contribution_id
 LEFT JOIN git_context_heads head ON head.repo_id=proof.repo_id AND head.id=proof.context_head_id
 LEFT JOIN git_head_receipts receipt ON receipt.repo_id=head.repo_id AND receipt.id=head.receipt_id`

func scanGitContributionProofPG(row pgx.Row, repo domain.ContentHash) (domain.GitContributionProof, error) {
	var id, admissionID, headID, contributionID, key, origin, baselineID, baselineOrigin string
	var raw, admissionRaw, baselineRaw, contributionRaw []byte
	var baselineVersion int64
	var headRow gitContextHeadRowPG
	columns := []any{&id, &admissionID, &headID, &contributionID, &raw, &key, &origin, &baselineID, &admissionRaw, &baselineOrigin, &baselineVersion, &baselineRaw, &contributionRaw}
	if err := row.Scan(append(columns, headRow.columns()...)...); err != nil {
		return domain.GitContributionProof{}, mapNoRows(err)
	}
	proof, err := decodePRHistory[domain.GitContributionProof](raw)
	if err != nil {
		return proof, err
	}
	if err := validateGitContributionProofIdentity(proof, repo, id, admissionID, headID, contributionID); err != nil {
		return domain.GitContributionProof{}, err
	}
	admission, err := decodeGitContributionAdmissionPG(admissionRaw, repo, admissionID, key, origin, baselineID)
	if err != nil {
		return domain.GitContributionProof{}, err
	}
	baseline, err := decodePRHistory[domain.GitHeadReceipt](baselineRaw)
	if err != nil || validateGitContributionProofBaseline(admission, baseline, repo, baselineOrigin, baselineVersion, baselineID) != nil {
		return domain.GitContributionProof{}, domain.ErrIntegrity
	}
	head, receipt, err := headRow.decode(repo)
	if err != nil {
		return domain.GitContributionProof{}, err
	}
	contribution, err := decodePRHistory[domain.GitContribution](contributionRaw)
	if err != nil || validateGitContributionProofInputs(proof, admission, head, receipt, contribution) != nil {
		return domain.GitContributionProof{}, domain.ErrIntegrity
	}
	return proof, nil
}

func (s *PostgresStore) PutGitContributionProof(ctx context.Context, proof domain.GitContributionProof) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if proof.ID != "" && proof.ID != proof.WithID().ID {
		return domain.ErrIntegrity
	}
	if err := proof.Validate(); err != nil {
		return err
	}
	repo := proof.Contribution.RepoID
	parent, err := s.gitContextWriteTransaction(ctx, repo, proof.Contribution.GitOrigin)
	if err != nil {
		return err
	}
	transaction, err := parent.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	admission, err := readGitContributionAdmissionPG(ctx, transaction, repo, proof.AdmissionID)
	if err != nil {
		return err
	}
	head, receipt, err := readGitContextHeadPG(ctx, transaction, repo, proof.ContextHeadID)
	if err != nil {
		return err
	}
	contribution, err := readGitContributionPG(ctx, transaction, repo, proof.Contribution.ID)
	if err != nil {
		return err
	}
	if contribution != proof.Contribution || !proof.MatchesInputs(admission, head, receipt) {
		return domain.ErrIntegrity
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO git_contribution_proofs(repo_id,id,admission_id,context_head_id,contribution_id,payload) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, repo, proof.ID, proof.AdmissionID, proof.ContextHeadID, contribution.ID, raw); err != nil {
		return err
	}
	old, err := findGitContributionProofPG(ctx, transaction, repo, proof.AdmissionID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrConflict
	}
	if err != nil {
		return err
	}
	if old != proof {
		return domain.ErrConflict
	}
	if err := enqueueGitProofSupplementsPG(ctx, transaction, proof); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func findGitContributionProofPG(ctx context.Context, database catalogMerkleReader, repo domain.ContentHash, admissionID string) (domain.GitContributionProof, error) {
	proof, err := scanGitContributionProofPG(database.QueryRow(ctx, gitContributionProofQueryPG+` WHERE proof.repo_id=$1 AND proof.admission_id=$2`, repo, admissionID), repo)
	if err == nil && proof.AdmissionID != admissionID {
		return domain.GitContributionProof{}, domain.ErrIntegrity
	}
	return proof, err
}

func (s *PostgresStore) FindGitContributionProof(ctx context.Context, repo domain.ContentHash, admissionID string) (domain.GitContributionProof, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitContributionProof{}, err
	}
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(domain.ContentHash("sha256:"+admissionID)) != nil {
		return domain.GitContributionProof{}, domain.ErrValidation
	}
	return findGitContributionProofPG(ctx, s.db(ctx), repo, admissionID)
}

func (s *PostgresStore) ListGitContributionProofs(ctx context.Context, repo domain.ContentHash) ([]domain.GitContributionProof, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, gitContributionProofQueryPG+` WHERE proof.repo_id=$1 ORDER BY proof.id`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.GitContributionProof{}
	for rows.Next() {
		proof, err := scanGitContributionProofPG(rows, repo)
		if err != nil {
			return nil, err
		}
		result = append(result, proof)
	}
	return result, rows.Err()
}
