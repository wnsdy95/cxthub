package store

import (
	"context"
	"os"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *FSStore) readDocObject(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	if err := validateHashes(repo, hash); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(s.docPath(repo, hash))
	if os.IsNotExist(err) {
		return nil, domain.ErrNotFound
	}
	return raw, err
}

func (s *FSStore) readDocBytes(ctx context.Context, repo, hash domain.ContentHash) ([]byte, bool, error) {
	raw, err := s.readDocObject(ctx, repo, hash)
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
	raw, err := s.readDocObject(ctx, repo, hash)
	if err != nil {
		return domain.VerifiedDocReference{}, err
	}
	return s.docProofs.verifyStored(ctx, repo, hash, raw, func(_ context.Context, ch domain.ContentHash) ([]byte, error) {
		raw, err := os.ReadFile(s.chunkPath(repo, ch))
		if os.IsNotExist(err) {
			return nil, domain.ErrNotFound
		}
		return raw, err
	})
}

var _ outbound.StoredDocVerifier = (*FSStore)(nil)

func (s *FSStore) CaptureSupersedes(ctx context.Context, repo, old, next domain.ContentHash, provider domain.ProviderKind, session string) (bool, error) {
	return compareStoredCaptures(ctx, s, &s.docProofs, repo, old, next, provider, session)
}

var _ outbound.StoredCaptureComparator = (*FSStore)(nil)
