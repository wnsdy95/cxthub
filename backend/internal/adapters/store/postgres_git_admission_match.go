//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitAdmissionMatchStore = (*PostgresStore)(nil)

func decodeGitAdmissionMatchPG(raw []byte, scope domain.GitAdmissionMatch, version int64) (domain.GitAdmissionMatch, error) {
	match, err := decodePRHistory[domain.GitAdmissionMatch](raw)
	if err != nil {
		return domain.GitAdmissionMatch{}, err
	}
	if match.RepoID != scope.RepoID || match.GitOrigin != scope.GitOrigin || match.Provider != scope.Provider || match.Page != scope.Page || match.Version != version || version <= 0 {
		return domain.GitAdmissionMatch{}, domain.ErrIntegrity
	}
	return match, nil
}

func (s *PostgresStore) GetGitAdmissionMatch(ctx context.Context, repo domain.ContentHash, origin string, provider domain.GitProviderObservation, page int) (domain.GitAdmissionMatch, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitAdmissionMatch{}, err
	}
	scope := domain.GitAdmissionMatch{RepoID: repo, GitOrigin: origin, Provider: provider, Page: page}
	if err := scope.Validate(); err != nil {
		return domain.GitAdmissionMatch{}, err
	}
	var raw []byte
	var version int64
	if err := s.db(ctx).QueryRow(ctx, `SELECT version,payload FROM git_admission_match WHERE repo_id=$1 AND origin=$2 AND installation_id=$3 AND provider_repository_id=$4 AND page=$5`, repo, origin, provider.InstallationID, provider.RepositoryID, page).Scan(&version, &raw); err != nil {
		return domain.GitAdmissionMatch{}, mapNoRows(err)
	}
	return decodeGitAdmissionMatchPG(raw, scope, version)
}

func (s *PostgresStore) GitAssociationPublicationUpperID(ctx context.Context, repo domain.ContentHash, origin string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validateGitHeadReceiptScope(repo, origin, 0); err != nil {
		return "", err
	}
	var upper string
	err := s.db(ctx).QueryRow(ctx, `SELECT COALESCE(MAX(publication_id),'') FROM git_association_publications WHERE repo_id=$1 AND origin=$2`, repo, origin).Scan(&upper)
	return upper, err
}

func validateGitAdmissionMatchReceiptPG(ctx context.Context, transaction pgx.Tx, receipt domain.GitHeadReceipt) error {
	var raw []byte
	if err := transaction.QueryRow(ctx, `SELECT payload FROM git_head_scans WHERE repo_id=$1 AND origin=$2 FOR UPDATE`, receipt.RepoID, receipt.GitOrigin).Scan(&raw); err != nil {
		return mapNoRows(err)
	}
	claim, err := decodePRHistory[domain.GitHeadScan](raw)
	if err != nil {
		return err
	}
	if claim.RepoID != receipt.RepoID || claim.GitOrigin != receipt.GitOrigin {
		return domain.ErrIntegrity
	}
	if claim.Version != receipt.Version || !claim.LeaseUntil.IsZero() || claim.Reason != "" {
		return domain.ErrConflict
	}
	if receipt.More {
		if claim.State != "waiting" || claim.Page <= 1 || claim.Page-1 != receipt.Page {
			return domain.ErrConflict
		}
	} else if claim.State != "completed" || claim.Page != 1 {
		return domain.ErrConflict
	}
	original, err := readGitHeadReceiptPG(ctx, transaction, receipt.RepoID, receipt.GitOrigin, receipt.Version)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(original, receipt) {
		return domain.ErrIntegrity
	}
	return nil
}

func (s *PostgresStore) SaveGitAdmissionMatch(ctx context.Context, next domain.GitAdmissionMatch, expectedVersion int64, receipt domain.GitHeadReceipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if expectedVersion < 0 || expectedVersion == math.MaxInt64 || next.Version != expectedVersion+1 {
		return domain.ErrConflict
	}
	if err := receipt.Validate(); err != nil {
		return err
	}
	if receipt.RepoID != next.RepoID || receipt.GitOrigin != next.GitOrigin || receipt.Provider != next.Provider || receipt.Page != next.Page {
		return domain.ErrIntegrity
	}
	parent, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || parent.owner != s || parent.repo != next.RepoID || parent.readOnly || parent.Tx == nil || parent.Conn().PgConn().TxStatus() != 'T' {
		return domain.ErrConflict
	}
	if err := s.checkRepositoryDocumentIdentity(ctx, parent, next.RepoID, true); err != nil {
		return err
	}
	metadata, err := s.GetRepo(ctx, next.RepoID)
	if err != nil {
		return err
	}
	if metadata.GitRemoteURL != next.GitOrigin {
		return domain.ErrConflict
	}
	transaction, err := parent.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if err := validateGitAdmissionMatchReceiptPG(ctx, transaction, receipt); err != nil {
		return err
	}
	var raw []byte
	var version int64
	err = transaction.QueryRow(ctx, `SELECT version,payload FROM git_admission_match WHERE repo_id=$1 AND origin=$2 AND installation_id=$3 AND provider_repository_id=$4 AND page=$5 FOR UPDATE`, next.RepoID, next.GitOrigin, next.Provider.InstallationID, next.Provider.RepositoryID, next.Page).Scan(&version, &raw)
	missing := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !missing {
		return err
	}
	if missing {
		if expectedVersion != 0 {
			return domain.ErrConflict
		}
	} else {
		if _, err := decodeGitAdmissionMatchPG(raw, next, version); err != nil {
			return err
		}
		if version != expectedVersion {
			return domain.ErrConflict
		}
	}
	raw, err = json.Marshal(next)
	if err != nil {
		return err
	}
	if missing {
		inserted, err := transaction.Exec(ctx, `INSERT INTO git_admission_match(repo_id,origin,installation_id,provider_repository_id,page,version,payload) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, next.RepoID, next.GitOrigin, next.Provider.InstallationID, next.Provider.RepositoryID, next.Page, next.Version, raw)
		if err != nil {
			return err
		}
		if inserted.RowsAffected() != 1 {
			return domain.ErrConflict
		}
	} else {
		updated, err := transaction.Exec(ctx, `UPDATE git_admission_match SET version=$6,payload=$7 WHERE repo_id=$1 AND origin=$2 AND installation_id=$3 AND provider_repository_id=$4 AND page=$5 AND version=$8`, next.RepoID, next.GitOrigin, next.Provider.InstallationID, next.Provider.RepositoryID, next.Page, next.Version, raw, expectedVersion)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return domain.ErrConflict
		}
	}
	return transaction.Commit(ctx)
}
