//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitHeadReceiptStore = (*PostgresStore)(nil)

func readGitHeadReceiptPG(ctx context.Context, database catalogMerkleReader, repo domain.ContentHash, origin string, version int64) (domain.GitHeadReceipt, error) {
	var raw []byte
	var id string
	if err := database.QueryRow(ctx, `SELECT id,payload FROM git_head_receipts WHERE repo_id=$1 AND origin=$2 AND version=$3`, repo, origin, version).Scan(&id, &raw); err != nil {
		return domain.GitHeadReceipt{}, mapNoRows(err)
	}
	receipt, err := decodePRHistory[domain.GitHeadReceipt](raw)
	if err != nil {
		return domain.GitHeadReceipt{}, err
	}
	if err := validateGitHeadReceiptIdentity(receipt, repo, origin, version); err != nil || receipt.ID != id {
		return domain.GitHeadReceipt{}, domain.ErrIntegrity
	}
	return receipt, nil
}

func putGitHeadReceiptPG(ctx context.Context, transaction pgx.Tx, receipt domain.GitHeadReceipt) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO git_head_receipts(repo_id,origin,version,id,payload) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, receipt.RepoID, receipt.GitOrigin, receipt.Version, receipt.ID, raw); err != nil {
		return err
	}
	old, err := readGitHeadReceiptPG(ctx, transaction, receipt.RepoID, receipt.GitOrigin, receipt.Version)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(old, receipt) {
		return domain.ErrConflict
	}
	return nil
}

func (s *PostgresStore) GetGitHeadReceipt(ctx context.Context, repo domain.ContentHash, origin string, version int64) (domain.GitHeadReceipt, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitHeadReceipt{}, err
	}
	if err := validateGitHeadReceiptScope(repo, origin, version); err != nil || version == 0 {
		return domain.GitHeadReceipt{}, domain.ErrValidation
	}
	return readGitHeadReceiptPG(ctx, s.db(ctx), repo, origin, version)
}

func (s *PostgresStore) ListGitHeadReceipts(ctx context.Context, repo domain.ContentHash, origin string, afterVersion int64, limit int) ([]domain.GitHeadReceipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateGitHeadReceiptScope(repo, origin, afterVersion); err != nil || limit < 1 || limit > 101 {
		return nil, domain.ErrValidation
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT version,id,payload FROM git_head_receipts WHERE repo_id=$1 AND origin=$2 AND version>$3 ORDER BY version LIMIT $4`, repo, origin, afterVersion, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.GitHeadReceipt{}
	for rows.Next() {
		var version int64
		var id string
		var raw []byte
		if err := rows.Scan(&version, &id, &raw); err != nil {
			return nil, err
		}
		receipt, err := decodePRHistory[domain.GitHeadReceipt](raw)
		if err != nil {
			return nil, err
		}
		if err := validateGitHeadReceiptIdentity(receipt, repo, origin, version); err != nil || receipt.ID != id || version <= afterVersion {
			return nil, domain.ErrIntegrity
		}
		result = append(result, receipt)
		afterVersion = version
	}
	return result, rows.Err()
}
