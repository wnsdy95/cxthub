//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *PostgresStore) readDocBytes(ctx context.Context, repo, hash domain.ContentHash) ([]byte, bool, error) {
	if err := validateHashes(repo, hash); err != nil {
		return nil, false, err
	}
	var raw []byte
	if err := s.db(ctx).QueryRow(ctx, `SELECT b.bytes FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind='doc' AND rb.hash=$2`, string(repo), string(hash)).Scan(&raw); err != nil {
		return nil, false, mapNoRows(err)
	}
	data, err := docDecompress(raw)
	if err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if body, chunked, err := s.getDocChunkedPG(ctx, repo, hash, data); chunked {
		return body, true, err
	}
	return data, false, nil
}
func (s *PostgresStore) VerifyStoredDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	raw, _, err := s.readDocBytes(ctx, repo, hash)
	if err != nil {
		return domain.VerifiedDocReference{}, err
	}
	return s.docProofs.verify(ctx, repo, hash, raw)
}

var _ outbound.StoredDocVerifier = (*PostgresStore)(nil)
