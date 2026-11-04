//go:build postgres

package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func decodeDocJobLifetimePG(raw []byte, epoch string) (domain.DocFinalizationJob, error) {
	var job domain.DocFinalizationJob
	if err := json.Unmarshal(raw, &job); err != nil {
		return job, err
	}
	var lifetime pgtype.UUID
	if lifetime.Scan(epoch) != nil || !lifetime.Valid || lifetime.Bytes == [16]byte{} {
		return job, domain.ErrIntegrity
	}
	job.RepositoryEpoch = epoch
	return job, nil
}

type docJobLifetimeReaderPG interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func retainDocJobRepositoryPG(ctx context.Context, database docJobLifetimeReaderPG, repo domain.ContentHash) (string, error) {
	return retainDocJobRepositoryLockPG(ctx, database, repo, false)
}

func retainDocJobRepositoryLockPG(ctx context.Context, database docJobLifetimeReaderPG, repo domain.ContentHash, nonblocking bool) (string, error) {
	locking := " FOR KEY SHARE"
	if nonblocking {
		locking += " NOWAIT"
	}
	var retained domain.ContentHash
	if err := database.QueryRow(ctx, `SELECT id FROM repos WHERE id=$1`+locking, repo).Scan(&retained); err != nil {
		return "", mapNoRows(err)
	}
	var epoch string
	if err := database.QueryRow(ctx, `SELECT epoch::text FROM repository_catalog_state WHERE repo_id=$1`+locking, repo).Scan(&epoch); err != nil {
		return "", mapNoRows(err)
	}
	return epoch, nil
}

func readLockedDocJobPG(ctx context.Context, database docJobLifetimeReaderPG, repo domain.ContentHash, identifier string) (domain.DocFinalizationJob, error) {
	epoch, err := retainDocJobRepositoryPG(ctx, database, repo)
	if err != nil {
		return domain.DocFinalizationJob{}, err
	}
	var raw []byte
	if err := database.QueryRow(ctx, `SELECT payload FROM doc_finalization_jobs WHERE repo_id=$1 AND id=$2 FOR UPDATE`, repo, identifier).Scan(&raw); err != nil {
		return domain.DocFinalizationJob{}, mapNoRows(err)
	}
	return decodeDocJobLifetimePG(raw, epoch)
}
