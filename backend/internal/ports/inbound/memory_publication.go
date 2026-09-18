package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// MemoryPublication restores one locally retained archive after transient
// capture collection. No refs, pending pointers or graft registers are changed.
type MemoryPublication struct {
	Objects CommitInput
	Memory  domain.MemoryDigest
}

type MemoryPublisher interface {
	PublishMemoryArchive(context.Context, MemoryPublication) (domain.ContentHash, error)
}
