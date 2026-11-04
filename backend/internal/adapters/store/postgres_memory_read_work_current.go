//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func memoryReadWorkUnavailable(err error) bool {
	return errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrMemoryCaptureExpired)
}

func (storage *PostgresStore) currentMemoryReadInTx(ctx context.Context, transaction pgx.Tx, target domain.MemoryReadTarget, identifier domain.ContentHash) (domain.MemoryReadHeader, *domain.MemoryReadChunkVerifier, error) {
	header, verifier, err := storage.qualifiedMemoryReadInTx(ctx, transaction, target.RepoID, identifier)
	if err != nil {
		return domain.MemoryReadHeader{}, nil, err
	}
	if header.Source.Graph.RepoID != target.RepoID || header.Rules != target.Rules || header.View.Selection != target.Selection || header.View.Content != target.Content {
		return domain.MemoryReadHeader{}, nil, domain.ErrIntegrity
	}
	return header, verifier, nil
}

func (storage *PostgresStore) WithCurrentMemoryReadRange(ctx context.Context, target domain.MemoryReadTarget, authorize func(context.Context) (context.Context, error), consume func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	if authorize == nil || consume == nil {
		return domain.ErrValidation
	}
	identifier, err := domain.MemoryReadTargetID(ctx, target)
	if err != nil {
		return err
	}
	if storage != nil && storage.InTransaction(ctx) {
		return domain.ErrConflict
	}
	transaction, _, err := storage.memoryReadSnapshot(ctx, target.RepoID, identifier)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	snapshot := &repositoryTx{Tx: transaction, owner: storage, readOnly: true, repo: target.RepoID}
	ctx, err = authorize(context.WithValue(ctx, repositoryTxKey{}, snapshot))
	if err != nil {
		return err
	}
	if ctx == nil || ctx.Value(repositoryTxKey{}) != snapshot {
		return domain.ErrConflict
	}
	work, err := scanMemoryReadWork(ctx, transaction.QueryRow(ctx, `SELECT `+memoryReadWorkColumns+`
 FROM memory_read_work job WHERE repo_id=$1 AND target_id=$2
 AND EXISTS(SELECT 1 FROM memory_read_work_scopes scope WHERE scope.repo_id=job.repo_id AND scope.enabled)`,
		string(target.RepoID), string(identifier)), target.RepoID, identifier)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrMemoryReadNotReady
	}
	if err != nil {
		return err
	}
	unavailable := domain.ErrMemoryReadNotReady
	if work.State == "rejected" {
		unavailable = domain.ErrMemoryReadNeedsAttention
	}
	if work.QualificationID == "" {
		return unavailable
	}
	header, verifier, err := storage.currentMemoryReadInTx(ctx, transaction, target, work.QualificationID)
	if memoryReadWorkUnavailable(err) {
		return unavailable
	}
	if err != nil {
		return err
	}
	raw, err := domain.MemoryReadHeaderBytes(ctx, header)
	if err != nil {
		return err
	}
	if err := consumeMemoryReadRange(ctx, transaction, target.RepoID, domain.HashContent(raw), header, verifier, consume); err != nil {
		if errors.Is(err, domain.ErrMemoryCaptureExpired) {
			return unavailable
		}
		return err
	}
	return transaction.Commit(ctx)
}
