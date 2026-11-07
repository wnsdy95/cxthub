//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func graphProjectionNeedsInputReset(ctx context.Context, transaction pgx.Tx, work projectionWork) (bool, error) {
	if work.Cursor != "" {
		cursor, err := decodeRepositoryViewSourceCursor(work.Cursor)
		if err != nil {
			return false, err
		}
		if cursor.Profile != domain.RepositoryGraphProjectionSourceProfile {
			return true, nil
		}
	}
	var obsolete bool
	err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM graph_projection_inputs
		WHERE repo_id=$1 AND dataset=$2 AND kind IN ('git_tree','git_delta'))`,
		string(work.RepoID), work.Dataset).Scan(&obsolete)
	return obsolete, err
}
