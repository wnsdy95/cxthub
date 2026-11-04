//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *PostgresStore) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	if length <= 0 || length > domain.ConversationManifestChunkBytes {
		return nil, domain.ErrIntegrity
	}
	raw, err := s.readOwnedDocObjectBounded(ctx, repo, "chunk", hash, int(2*length)+64)
	if err != nil {
		return nil, err
	}
	return rootChunkBytes(ctx, raw, length)
}

var _ outbound.ConversationChunkReader = (*PostgresStore)(nil)
