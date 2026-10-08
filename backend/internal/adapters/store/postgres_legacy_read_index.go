//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// The current descriptor selects the identity, even if an old projection is
// present and the snapshot is missing or mislabeled. Root readers use their
// owned proof instead of this legacy projection path.
func (s *PostgresStore) legacyDocIndexReady(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	var raw []byte
	var ready bool
	err := s.db(ctx).QueryRow(ctx, `SELECT b.bytes,EXISTS(SELECT 1 FROM doc_read_index_current WHERE hash=$2)
 FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind='doc' AND rb.hash=$2`, string(repo), string(hash)).Scan(&raw, &ready)
	if err != nil {
		return false, mapNoRows(err)
	}
	if err := rejectStoredRoot(ctx, raw); err != nil {
		return false, err
	}
	return ready, nil
}
