//go:build postgres

package store

import (
	"bytes"
	"context"
	"slices"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type postgresDocRangeRead struct {
	transaction *repositoryTx
	repo        domain.ContentHash
	index       domain.DocRangeIndex
}

func (s *PostgresStore) OpenDocRange(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeRead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !s.InReadOnlyTransaction(ctx) {
		return nil, domain.ErrConflict
	}
	header, err := s.DocRangeIndex(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	return &postgresDocRangeRead{transaction: ctx.Value(repositoryTxKey{}).(*repositoryTx), repo: repo, index: header}, nil
}

func (read *postgresDocRangeRead) Header() domain.DocRangeIndex {
	header := read.index
	header.Pages = slices.Clone(header.Pages)
	header.Manifest.Envelope = bytes.Clone(header.Manifest.Envelope)
	header.Manifest.Chunks = slices.Clone(header.Manifest.Chunks)
	return header
}

func (read *postgresDocRangeRead) ReadPage(ctx context.Context, position int) ([]domain.DocRangeEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bound, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || bound != read.transaction {
		return nil, domain.ErrConflict
	}
	if position < 0 || position >= len(read.index.Pages) {
		return nil, domain.ErrIntegrity
	}
	var raw []byte
	err := bound.QueryRow(ctx, `SELECT substring(p.bytes FROM 1 FOR $4)
 FROM repo_blobs rb LEFT JOIN doc_range_pages_v1 p ON p.doc_hash=rb.hash AND p.ordinal=$3
 WHERE rb.repo_id=$1 AND rb.kind='doc' AND rb.hash=$2`, read.repo, read.index.Hash, position, domain.MaxDocRangePageBytes+1).Scan(&raw)
	if err != nil {
		return nil, mapNoRows(err)
	}
	return read.index.DecodePage(position, raw)
}

var _ outbound.DocRangeSnapshotStore = (*PostgresStore)(nil)
