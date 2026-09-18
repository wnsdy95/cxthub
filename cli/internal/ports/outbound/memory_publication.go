package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// MemoryArchivePublisher atomically restores a snapshot/document and attaches
// its root memory. Optional capability: old peers fail closed on collection.
type MemoryArchivePublisher interface {
	PublishMemoryArchive(context.Context, string, domain.Snapshot, domain.SessionDoc, domain.MemoryDigest) error
}
