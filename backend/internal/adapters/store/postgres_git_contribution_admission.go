//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitContributionAdmissionStore = (*PostgresStore)(nil)

func (s *PostgresStore) FenceGitHeadRead(ctx context.Context, repo domain.ContentHash, origin string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := validateGitHeadReceiptScope(repo, origin, 0); err != nil {
		return 0, err
	}
	transaction, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || transaction.owner != s || transaction.repo != repo || transaction.readOnly || transaction.Tx == nil || transaction.Conn().PgConn().TxStatus() != 'T' {
		return 0, domain.ErrConflict
	}
	if err := s.checkRepositoryDocumentIdentity(ctx, transaction, repo, true); err != nil {
		return 0, err
	}
	var raw []byte
	if err := transaction.QueryRow(ctx, `SELECT payload FROM git_head_scans WHERE repo_id=$1 AND origin=$2 FOR UPDATE`, repo, origin).Scan(&raw); err != nil {
		return 0, mapNoRows(err)
	}
	claim, err := decodePRHistory[domain.GitHeadScan](raw)
	if err != nil {
		return 0, err
	}
	if claim.RepoID != repo || claim.GitOrigin != origin {
		return 0, domain.ErrIntegrity
	}
	return claim.Version, nil
}

func decodeGitContributionAdmissionPG(raw []byte, repo domain.ContentHash, id, key, origin, receiptID string) (domain.GitContributionAdmission, error) {
	admission, err := decodePRHistory[domain.GitContributionAdmission](raw)
	if err != nil {
		return domain.GitContributionAdmission{}, err
	}
	if admission.ID != id || admission.Key() != key || admission.Destination.RepoID != repo || admission.Destination.GitOrigin != origin || admission.Destination.ReadReceiptID != receiptID {
		return domain.GitContributionAdmission{}, domain.ErrIntegrity
	}
	return admission, nil
}

func readGitContributionAdmissionPG(ctx context.Context, database catalogMerkleReader, repo domain.ContentHash, id string) (domain.GitContributionAdmission, error) {
	var raw []byte
	var key, origin, receiptID string
	if err := database.QueryRow(ctx, `SELECT admission_key,origin,receipt_id,payload FROM git_contribution_admissions WHERE repo_id=$1 AND id=$2`, repo, id).Scan(&key, &origin, &receiptID, &raw); err != nil {
		return domain.GitContributionAdmission{}, mapNoRows(err)
	}
	return decodeGitContributionAdmissionPG(raw, repo, id, key, origin, receiptID)
}

func (s *PostgresStore) PutGitContributionAdmission(ctx context.Context, admission domain.GitContributionAdmission) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if admission.ID != "" && admission.ID != admission.WithID().ID {
		return domain.ErrIntegrity
	}
	if err := admission.Validate(); err != nil {
		return err
	}
	destination := admission.Destination
	version, err := s.FenceGitHeadRead(ctx, destination.RepoID, destination.GitOrigin)
	if err != nil {
		return err
	}
	if version != admission.AfterReadVersion {
		return domain.ErrConflict
	}
	receipt, err := readGitHeadReceiptPG(ctx, s.db(ctx), destination.RepoID, destination.GitOrigin, destination.ReadVersion)
	if err != nil {
		return err
	}
	if !admission.MatchesBaseline(receipt) {
		return domain.ErrIntegrity
	}
	transaction, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	raw, err := json.Marshal(admission)
	if err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO git_contribution_admissions(repo_id,id,admission_key,origin,receipt_id,payload) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, destination.RepoID, admission.ID, admission.Key(), destination.GitOrigin, destination.ReadReceiptID, raw); err != nil {
		return err
	}
	old, err := readGitContributionAdmissionPG(ctx, transaction, destination.RepoID, admission.ID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrConflict
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(old, admission) {
		return domain.ErrConflict
	}
	if err := enqueueGitContributionWorkPG(ctx, transaction, admission); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func (s *PostgresStore) GetGitContributionAdmission(ctx context.Context, repo domain.ContentHash, id string) (domain.GitContributionAdmission, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitContributionAdmission{}, err
	}
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(domain.ContentHash("sha256:"+id)) != nil {
		return domain.GitContributionAdmission{}, domain.ErrValidation
	}
	return readGitContributionAdmissionPG(ctx, s.db(ctx), repo, id)
}

func (s *PostgresStore) FindGitContributionAdmission(ctx context.Context, repo domain.ContentHash, key string) (domain.GitContributionAdmission, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitContributionAdmission{}, err
	}
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(domain.ContentHash("sha256:"+key)) != nil {
		return domain.GitContributionAdmission{}, domain.ErrValidation
	}
	var raw []byte
	var id, origin, receiptID string
	if err := s.db(ctx).QueryRow(ctx, `SELECT id,origin,receipt_id,payload FROM git_contribution_admissions WHERE repo_id=$1 AND admission_key=$2`, repo, key).Scan(&id, &origin, &receiptID, &raw); err != nil {
		return domain.GitContributionAdmission{}, mapNoRows(err)
	}
	return decodeGitContributionAdmissionPG(raw, repo, id, key, origin, receiptID)
}

func (s *PostgresStore) ListGitContributionAdmissions(ctx context.Context, repo domain.ContentHash, origin, afterID string, limit int) ([]domain.GitContributionAdmission, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if domain.ValidateContentHash(repo) != nil || (origin != "" && validateGitHeadReceiptScope(repo, origin, 0) != nil) || (afterID != "" && domain.ValidateContentHash(domain.ContentHash("sha256:"+afterID)) != nil) || limit < 1 || limit > 101 {
		return nil, domain.ErrValidation
	}
	query := `SELECT id,admission_key,origin,receipt_id,payload FROM git_contribution_admissions WHERE repo_id=$1 AND id>$2`
	arguments := []any{repo, afterID, limit}
	if origin != "" {
		query += ` AND origin=$4`
		arguments = append(arguments, origin)
	}
	rows, err := s.db(ctx).Query(ctx, query+` ORDER BY id LIMIT $3`, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.GitContributionAdmission{}
	for rows.Next() {
		var id, key, storedOrigin, receiptID string
		var raw []byte
		if err := rows.Scan(&id, &key, &storedOrigin, &receiptID, &raw); err != nil {
			return nil, err
		}
		admission, err := decodeGitContributionAdmissionPG(raw, repo, id, key, storedOrigin, receiptID)
		if err != nil {
			return nil, err
		}
		if id <= afterID || (origin != "" && storedOrigin != origin) {
			return nil, domain.ErrIntegrity
		}
		result = append(result, admission)
		afterID = id
	}
	return result, rows.Err()
}
