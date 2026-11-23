package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// DocumentChunks describes an already verified canonical document. The ordered
// chunk IDs and envelope preserve its existing identity; they are not a new ID.
// ReadChunk is valid only during WithVerifiedDocChunks' callback. It accepts
// only listed IDs and verifies each requested body's current content hash.
type DocumentChunks struct {
	Representation domain.DocumentRepresentation
	ReadChunk      func(context.Context, domain.ContentHash) ([]byte, error)
}
