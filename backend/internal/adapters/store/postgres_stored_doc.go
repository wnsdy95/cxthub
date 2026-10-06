//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Ownership is checked through the caller's transaction for documents and
// chunks alike, even when a disposable physical proof is already cached.
func (s *PostgresStore) readOwnedDocObject(ctx context.Context, repo domain.ContentHash, kind string, hash domain.ContentHash) ([]byte, error) {
	if err := validateHashes(repo, hash); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var raw []byte
	if err := s.db(ctx).QueryRow(ctx, `SELECT b.bytes FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind=$2 AND rb.hash=$3`, string(repo), kind, string(hash)).Scan(&raw); err != nil {
		return nil, mapNoRows(err)
	}
	return raw, nil
}

func (s *PostgresStore) readDocBytes(ctx context.Context, repo, hash domain.ContentHash) ([]byte, bool, error) {
	raw, err := s.readOwnedDocObject(ctx, repo, "doc", hash)
	if err != nil {
		return nil, false, err
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
	raw, err := s.readOwnedDocObject(ctx, repo, "doc", hash)
	if err != nil {
		return domain.VerifiedDocReference{}, err
	}
	return s.docProofs.verifyStored(ctx, repo, hash, raw, func(ctx context.Context, ch domain.ContentHash) ([]byte, error) {
		return s.readOwnedDocObject(ctx, repo, "chunk", ch)
	})
}

var _ outbound.StoredDocVerifier = (*PostgresStore)(nil)

func (s *PostgresStore) CaptureSupersedes(ctx context.Context, repo, old, next domain.ContentHash, provider domain.ProviderKind, session string) (bool, error) {
	return compareStoredCaptures(ctx, s, &s.docProofs, repo, old, next, provider, session)
}

var _ outbound.StoredCaptureComparator = (*PostgresStore)(nil)
