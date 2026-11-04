package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type MemoryCaptureStore interface {
	WriteMemoryCapture(context.Context, domain.MemoryCaptureBundle, time.Time) error
	ReadMemoryCapture(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryCaptureBundle, error)
	PruneMemoryCaptures(context.Context, domain.ContentHash, int) (int, error)
}

type MemoryCaptureAnchorStore interface {
	FindMemoryCaptureAnchor(context.Context, domain.RepositoryViewSourceCheckpoint, time.Time) (domain.ContentHash, error)
}
