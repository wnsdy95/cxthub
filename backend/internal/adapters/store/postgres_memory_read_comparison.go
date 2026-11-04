//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.MemoryReadComparisonStore = (*PostgresStore)(nil)

func (storage *PostgresStore) RecordMemoryReadComparison(ctx context.Context, job domain.MemoryReadWork, comparison domain.MemoryReadComparison) (failure error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, docRangeEnqueueTimeout)
	defer cancel()
	defer func() { failure = memoryReadWorkError(parent, failure) }()
	if err := job.Validate(); err != nil {
		return err
	}
	if job.State != "running" {
		return domain.ErrValidation
	}
	raw, err := domain.MemoryReadComparisonBytes(ctx, comparison)
	if err != nil {
		return err
	}
	if comparison.TargetID != job.ID || comparison.Source.Graph.RepoID != job.Target.RepoID {
		return domain.ErrConflict
	}
	if err := domain.CheckMemorySourceMinimum(comparison.Source, job.Source); err != nil {
		return err
	}
	transaction, err := storage.beginMemoryReadWork(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if _, err := lockMemoryReadWork(ctx, transaction, job); err != nil {
		return err
	}
	if err := memoryReadWorkLive(ctx, transaction, job); err != nil {
		return err
	}
	repo := job.Target.RepoID
	var locked bool
	if err := transaction.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('cxt-memory-capture:' || $1,0))`, string(repo)).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return domain.ErrConflict
	}
	current, err := readMemoryQualificationSource(ctx, transaction, repo)
	if err != nil {
		return err
	}
	if err := domain.CheckMemorySourceMinimum(current, comparison.Source); err != nil {
		return err
	}
	var readID string
	if err := transaction.QueryRow(ctx, `SELECT read_id FROM memory_read_headers WHERE repo_id=$1 AND read_id=$2 FOR SHARE NOWAIT`, string(repo), string(comparison.ReadID)).Scan(&readID); err != nil {
		return mapNoRows(err)
	}
	header, _, err := readMemoryReadDescriptor(ctx, transaction, repo, comparison.ReadID)
	if err != nil {
		return err
	}
	if err := domain.ValidateMemoryReadComparison(ctx, comparison, header); err != nil {
		return err
	}
	var captureID string
	if err := transaction.QueryRow(ctx, `SELECT capture_id FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2 FOR SHARE NOWAIT`, string(repo), string(comparison.CaptureID)).Scan(&captureID); err != nil {
		return mapNoRows(err)
	}
	if err := memoryReadCheckExpiry(ctx, transaction, repo, comparison.CaptureID); err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO memory_read_comparisons(repo_id,read_id,body)
 VALUES($1,$2,$3) ON CONFLICT(repo_id,read_id) DO NOTHING`, string(repo), string(comparison.ReadID), raw); err != nil {
		return err
	}
	stored, _, err := readMemoryReadComparison(ctx, transaction, repo, comparison.ReadID, true)
	if err != nil {
		return err
	}
	if !bytes.Equal(stored, raw) {
		return domain.ErrConflict
	}
	if err := memoryReadCheckExpiry(ctx, transaction, repo, comparison.CaptureID); err != nil {
		return err
	}
	if err := memoryReadWorkLive(ctx, transaction, job); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func readMemoryReadComparison(ctx context.Context, transaction pgx.Tx, repo, readID domain.ContentHash, lock bool) ([]byte, domain.MemoryReadComparison, error) {
	query := `SELECT octet_length(body),CASE WHEN octet_length(body)<=$3 THEN body END
 FROM memory_read_comparisons WHERE repo_id=$1 AND read_id=$2`
	if lock {
		query += ` FOR SHARE NOWAIT`
	}
	var raw []byte
	var size int64
	var comparison domain.MemoryReadComparison
	if err := transaction.QueryRow(ctx, query, string(repo), string(readID), domain.MaxMemoryReadComparisonBytes).Scan(&size, &raw); err != nil {
		return nil, comparison, mapNoRows(err)
	}
	if size < 1 || size > domain.MaxMemoryReadComparisonBytes || int64(len(raw)) != size {
		return nil, comparison, domain.ErrIntegrity
	}
	if err := domain.DecodeRepositorySourceJSON(ctx, raw, &comparison); err != nil {
		if ctx.Err() != nil {
			return nil, comparison, ctx.Err()
		}
		return nil, comparison, domain.ErrIntegrity
	}
	canonical, err := domain.MemoryReadComparisonBytes(ctx, comparison)
	if ctx.Err() != nil {
		return nil, comparison, ctx.Err()
	}
	if err != nil || !bytes.Equal(canonical, raw) || comparison.ReadID != readID || comparison.Source.Graph.RepoID != repo {
		return nil, comparison, domain.ErrIntegrity
	}
	return raw, comparison, nil
}

func requireMemoryReadComparison(ctx context.Context, transaction pgx.Tx, readID, targetID domain.ContentHash, header domain.MemoryReadHeader, lock bool) error {
	_, comparison, err := readMemoryReadComparison(ctx, transaction, header.Source.Graph.RepoID, readID, lock)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrMemoryReadNotReady
	}
	if err != nil {
		return err
	}
	if comparison.TargetID != targetID {
		return domain.ErrIntegrity
	}
	if err := domain.ValidateMemoryReadComparison(ctx, comparison, header); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return domain.ErrIntegrity
	}
	if !comparison.Matched() {
		return domain.ErrMemoryReadComparisonMismatch
	}
	return ctx.Err()
}
