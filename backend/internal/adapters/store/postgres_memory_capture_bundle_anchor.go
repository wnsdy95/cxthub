//go:build postgres

package store

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.MemoryCaptureAnchorStore = (*PostgresStore)(nil)

const memoryCaptureFindAnchorSQL = `SELECT capture_id FROM memory_capture_headers
	WHERE repo_id=$1 AND graph_epoch=$2 AND graph_sequence<=$3
	AND capture_version=1 AND graph_anchor_id IS NULL
	AND expires_at>clock_timestamp() AND expires_at>=$4
	ORDER BY graph_sequence DESC,capture_id LIMIT 1`

func (storage *PostgresStore) FindMemoryCaptureAnchor(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, minimumExpiry time.Time) (domain.ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := checkpoint.Validate(); err != nil {
		return "", err
	}
	if checkpoint.Scope != domain.RepositoryGraphSourceScope || minimumExpiry.IsZero() {
		return "", domain.ErrValidation
	}
	minimumExpiry = minimumExpiry.UTC().Truncate(time.Microsecond)
	transaction, owned, err := storage.memoryReadSnapshot(ctx, checkpoint.RepoID, checkpoint.RepoID)
	if err != nil {
		return "", err
	}
	if owned {
		defer rollbackPG(transaction)
	}
	var identifier domain.ContentHash
	if err := transaction.QueryRow(ctx, memoryCaptureFindAnchorSQL, string(checkpoint.RepoID), checkpoint.Epoch, checkpoint.Sequence, minimumExpiry).
		Scan(&identifier); err != nil {
		return "", mapNoRows(err)
	}
	anchor, err := readMemoryCaptureHeader(ctx, transaction, checkpoint.RepoID, identifier, false)
	if err != nil {
		return "", err
	}
	actual := anchor.header.Generation.Graph
	if anchor.header.Version != domain.MemoryCaptureVersion || anchor.anchorID != "" || anchor.expiresAt.Before(minimumExpiry) ||
		actual.Version != checkpoint.Version || actual.Scope != checkpoint.Scope || actual.Epoch != checkpoint.Epoch || actual.Sequence > checkpoint.Sequence {
		return "", domain.ErrIntegrity
	}
	if owned {
		if err := transaction.Commit(ctx); err != nil {
			return "", err
		}
	}
	return identifier, nil
}
