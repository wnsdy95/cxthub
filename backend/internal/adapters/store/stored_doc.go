package store

import (
	"context"
	"os"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *FSStore) readDocBytes(ctx context.Context, repo, hash domain.ContentHash) ([]byte, bool, error) {
	if err := validateHashes(repo, hash); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	raw, err := os.ReadFile(s.docPath(repo, hash))
	if os.IsNotExist(err) {
		return nil, false, domain.ErrNotFound
	}
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
	if body, chunked, err := s.getDocChunkedContext(ctx, repo, hash, data); chunked {
		return body, true, err
	}
	return data, false, nil
}
func (s *FSStore) VerifyStoredDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	raw, _, err := s.readDocBytes(ctx, repo, hash)
	if err != nil {
		return domain.VerifiedDocReference{}, err
	}
	return s.docProofs.verify(ctx, repo, hash, raw)
}

var _ outbound.StoredDocVerifier = (*FSStore)(nil)
