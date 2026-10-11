//go:build postgres

package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitAssociationDiscoveryStore = (*PostgresStore)(nil)

func decodeGitAssociationDiscoveryPG(raw []byte, repo domain.ContentHash, origin string, version int64) (domain.GitAssociationDiscovery, error) {
	discovery, err := decodePRHistory[domain.GitAssociationDiscovery](raw)
	if err != nil {
		return domain.GitAssociationDiscovery{}, err
	}
	if discovery.RepoID != repo || discovery.GitOrigin != origin || discovery.Version != version || version <= 0 {
		return domain.GitAssociationDiscovery{}, domain.ErrIntegrity
	}
	return discovery, nil
}

func (s *PostgresStore) GetGitAssociationDiscovery(ctx context.Context, repo domain.ContentHash, origin string) (domain.GitAssociationDiscovery, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitAssociationDiscovery{}, err
	}
	if err := validateGitHeadReceiptScope(repo, origin, 0); err != nil {
		return domain.GitAssociationDiscovery{}, err
	}
	var raw []byte
	var version int64
	if err := s.db(ctx).QueryRow(ctx, `SELECT version,payload FROM git_association_discovery WHERE repo_id=$1 AND origin=$2`, repo, origin).Scan(&version, &raw); err != nil {
		return domain.GitAssociationDiscovery{}, mapNoRows(err)
	}
	return decodeGitAssociationDiscoveryPG(raw, repo, origin, version)
}

func decodeGitAssociationPublicationPG(raw []byte, repo domain.ContentHash, id string) (domain.HistoryEvent, error) {
	var publication domain.HistoryEvent
	if json.Unmarshal(raw, &publication) != nil || domain.ValidateHistoryEvent(publication) != nil || publication.Kind != "publish" || publication.RepoID != string(repo) || publication.ID != id {
		return domain.HistoryEvent{}, domain.ErrIntegrity
	}
	return publication, nil
}

func putGitAssociationPublicationPG(ctx context.Context, transaction pgx.Tx, repo domain.ContentHash, origin string, publication domain.HistoryEvent) error {
	raw, err := json.Marshal(publication)
	if err != nil {
		return err
	}
	var accepted []byte
	var same bool
	if err := transaction.QueryRow(ctx, `SELECT event,event=$3::jsonb FROM context_history WHERE repo_id=$1 AND id=$2 FOR SHARE`, repo, publication.ID, raw).Scan(&accepted, &same); err != nil {
		return mapNoRows(err)
	}
	if _, err := decodeGitAssociationPublicationPG(accepted, repo, publication.ID); err != nil {
		return err
	}
	if !same {
		return domain.ErrConflict
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO git_association_publications(repo_id,origin,publication_id,payload) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, repo, origin, publication.ID, accepted); err != nil {
		return err
	}
	if err := transaction.QueryRow(ctx, `SELECT payload=$4::jsonb FROM git_association_publications WHERE repo_id=$1 AND origin=$2 AND publication_id=$3`, repo, origin, publication.ID, accepted).Scan(&same); err != nil {
		return mapNoRows(err)
	}
	if !same {
		return domain.ErrConflict
	}
	return enqueueGitPublicationSupplementsPG(ctx, transaction, repo, origin, publication)
}

func (s *PostgresStore) SaveGitAssociationDiscovery(ctx context.Context, next domain.GitAssociationDiscovery, expectedVersion int64, publications []domain.HistoryEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if expectedVersion < 0 || expectedVersion == math.MaxInt64 || next.Version != expectedVersion+1 {
		return domain.ErrConflict
	}
	if len(publications) > domain.MaxCatalogLimit {
		return domain.ErrValidation
	}
	seen := make(map[string]bool, len(publications))
	for _, publication := range publications {
		if domain.ValidateHistoryEvent(publication) != nil || publication.Kind != "publish" || seen[publication.ID] {
			return domain.ErrValidation
		}
		if publication.RepoID != string(next.RepoID) {
			return domain.ErrIntegrity
		}
		seen[publication.ID] = true
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
	var raw []byte
	var version int64
	err = transaction.QueryRow(ctx, `SELECT version,payload FROM git_association_discovery WHERE repo_id=$1 AND origin=$2 FOR UPDATE`, next.RepoID, next.GitOrigin).Scan(&version, &raw)
	missing := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !missing {
		return err
	}
	if missing {
		if expectedVersion != 0 {
			return domain.ErrConflict
		}
	} else {
		if _, err := decodeGitAssociationDiscoveryPG(raw, next.RepoID, next.GitOrigin, version); err != nil {
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
		inserted, err := transaction.Exec(ctx, `INSERT INTO git_association_discovery(repo_id,origin,version,payload) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, next.RepoID, next.GitOrigin, next.Version, raw)
		if err != nil {
			return err
		}
		if inserted.RowsAffected() != 1 {
			return domain.ErrConflict
		}
	} else {
		updated, err := transaction.Exec(ctx, `UPDATE git_association_discovery SET version=$3,payload=$4 WHERE repo_id=$1 AND origin=$2 AND version=$5`, next.RepoID, next.GitOrigin, next.Version, raw, expectedVersion)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return domain.ErrConflict
		}
	}
	for _, publication := range publications {
		if err := putGitAssociationPublicationPG(ctx, transaction, next.RepoID, next.GitOrigin, publication); err != nil {
			return err
		}
	}
	return transaction.Commit(ctx)
}

func (s *PostgresStore) ListGitAssociationPublications(ctx context.Context, repo domain.ContentHash, origin, afterID string, limit int) ([]domain.HistoryEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if validateGitHeadReceiptScope(repo, origin, 0) != nil || limit < 1 || limit > 101 {
		return nil, domain.ErrValidation
	}
	if afterID != "" {
		if len(afterID) != 32 || afterID != strings.ToLower(afterID) {
			return nil, domain.ErrValidation
		}
		if _, err := hex.DecodeString(afterID); err != nil {
			return nil, domain.ErrValidation
		}
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT repo_id,origin,publication_id,payload FROM git_association_publications WHERE repo_id=$1 AND origin=$2 AND publication_id>$3 ORDER BY publication_id LIMIT $4`, repo, origin, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.HistoryEvent{}
	for rows.Next() {
		var storedRepo domain.ContentHash
		var storedOrigin, id string
		var raw []byte
		if err := rows.Scan(&storedRepo, &storedOrigin, &id, &raw); err != nil {
			return nil, err
		}
		if storedRepo != repo || storedOrigin != origin || id <= afterID {
			return nil, domain.ErrIntegrity
		}
		publication, err := decodeGitAssociationPublicationPG(raw, repo, id)
		if err != nil {
			return nil, err
		}
		result = append(result, publication)
		afterID = id
	}
	return result, rows.Err()
}
