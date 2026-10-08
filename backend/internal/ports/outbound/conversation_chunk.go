package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// ConversationChunkReader reads current repository-owned bytes, with decoded
// output bounded by the manifest length before allocation/decompression. It
// reports malformed storage as ErrIntegrity, preserving I/O and cancellation
// errors. Root verification must never fall back to the legacy GetChunk path.
type ConversationChunkReader interface {
	ReadConversationChunk(context.Context, domain.ContentHash, domain.ContentHash, int64) ([]byte, error)
}
