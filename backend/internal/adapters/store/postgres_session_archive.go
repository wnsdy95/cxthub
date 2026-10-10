//go:build postgres

package store

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.SessionArchiveStore = (*PostgresStore)(nil)

func (s *PostgresStore) ListSessionArchives(ctx context.Context, repo domain.ContentHash) ([]domain.SessionArchive, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT repo_id,key,snapshot_id,provider,session_id,archived_at,archived_by
 FROM session_archives WHERE repo_id=$1 ORDER BY key`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []domain.SessionArchive{}
	for rows.Next() {
		var record domain.SessionArchive
		if err := rows.Scan(&record.RepoID, &record.Key, &record.SnapshotID, &record.Provider, &record.SessionID, &record.ArchivedAt, &record.ArchivedBy); err != nil {
			return nil, err
		}
		if err := validateSessionArchive(record); err != nil {
			return nil, fmt.Errorf("%w: invalid stored session archive: %v", domain.ErrIntegrity, err)
		}
		if record.RepoID != repo {
			return nil, domain.ErrIntegrity
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func (s *PostgresStore) PutSessionArchive(ctx context.Context, record domain.SessionArchive) error {
	if err := validateSessionArchive(record); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO session_archives(repo_id,key,snapshot_id,provider,session_id,archived_at,archived_by)
 VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (repo_id,key) DO NOTHING`,
		record.RepoID, record.Key, record.SnapshotID, record.Provider, record.SessionID, record.ArchivedAt, record.ArchivedBy)
	return err
}

func (s *PostgresStore) DeleteSessionArchive(ctx context.Context, repo, key domain.ContentHash) error {
	if err := validateHashes(repo, key); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM session_archives WHERE repo_id=$1 AND key=$2`, repo, key)
	return err
}
