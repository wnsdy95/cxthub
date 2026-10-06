package outbound

import (
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// DocumentChunks describes an already verified canonical document. The ordered
// chunk IDs and envelope preserve its existing identity; they are not a new ID.
// ReadChunk is valid only during WithVerifiedDocChunks' callback. It accepts
// only listed IDs and verifies each requested body's current content hash.
type DocumentChunks struct {
	Hash      domain.ContentHash
	Format    string
	Envelope  json.RawMessage
	Chunks    []domain.ContentHash
	ReadChunk func(context.Context, domain.ContentHash) ([]byte, error)
}

// ChunkedDocumentStore exposes a descriptor bound to the exact representation
// whose complete canonical identity and CIR semantics were verified. Objects
// remain retained throughout use. Authenticated validation receipts may be
// reused only after checking the current stored bytes.
//
// False, nil means the representation is unsupported and use was not called.
// All errors, including corruption and cancellation, must propagate without
// fallback. The callback is synchronous and must not retain ReadChunk.
type ChunkedDocumentStore interface {
	WithVerifiedDocChunks(ctx context.Context, hash domain.ContentHash, use func(DocumentChunks) error) (bool, error)
}

// ChunkedDocumentPusher negotiates and uploads missing bounded chunks before
// finalizing one document. It never publishes snapshots, refs, or pendings.
// False, nil means an unsupported peer, with no writes performed. Any other
// failure must propagate; partial uploads must never trigger a legacy retry.
type ChunkedDocumentPusher interface {
	PushDocChunks(ctx context.Context, repoID string, doc DocumentChunks) (bool, error)
}
