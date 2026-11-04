//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func retainDocRangeJobRepositoryPG(ctx context.Context, transaction pgx.Tx, job domain.DocRangeIndexJob) error {
	var claimed pgtype.UUID
	if claimed.Scan(job.RepositoryEpoch) != nil || !claimed.Valid || claimed.Bytes == [16]byte{} {
		return domain.ErrConflict
	}
	epoch, err := retainDocJobRepositoryPG(ctx, transaction, job.RepoID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrConflict
	}
	if err != nil {
		return err
	}
	if epoch != job.RepositoryEpoch {
		return domain.ErrConflict
	}
	return ctx.Err()
}

func (storage *PostgresStore) beginBoundDocRangeJob(ctx context.Context, job domain.DocRangeIndexJob) (pgx.Tx, error) {
	transaction, err := storage.beginDocRangeJob(ctx)
	if err != nil {
		return nil, err
	}
	if err := retainDocRangeJobRepositoryPG(ctx, transaction, job); err != nil {
		rollbackPG(transaction)
		return nil, err
	}
	return transaction, nil
}
