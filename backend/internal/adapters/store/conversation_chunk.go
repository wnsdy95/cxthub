package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *FSStore) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	if err := validateHashes(repo, hash); err != nil {
		return nil, err
	}
	if length <= 0 || length > domain.ConversationManifestChunkBytes {
		return nil, domain.ErrIntegrity
	}
	raw, err := s.ownedFSChunkReader(repo)(ctx, hash)
	if err != nil {
		return nil, err
	}
	return rootChunkBytes(ctx, raw, length)
}

var _ outbound.ConversationChunkReader = (*FSStore)(nil)
