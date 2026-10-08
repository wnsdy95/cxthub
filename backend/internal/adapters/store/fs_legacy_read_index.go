package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *FSStore) requireLegacyIndexedDoc(ctx context.Context, repo, hash domain.ContentHash) error {
	raw, err := s.readDocObject(ctx, repo, hash)
	if err != nil {
		return err
	}
	return rejectStoredRoot(ctx, raw)
}
