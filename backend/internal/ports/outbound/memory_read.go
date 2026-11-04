package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type MemoryReadStore interface {
	MemoryReadRangeStore
	WriteMemoryRead(context.Context, domain.MemoryReadBundle) error
	ReadMemoryReadHeader(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryReadHeader, error)
	ReadMemoryReadChunk(context.Context, domain.ContentHash, domain.ContentHash, int) ([]byte, error)
}

type MemoryReadRangeStore interface {
	WithMemoryReadRange(context.Context, domain.ContentHash, domain.ContentHash, func(context.Context, domain.MemoryReadHeader, MemoryReadChunkReader) error) error
}

type MemoryReadChunkReader interface {
	ReadChunk(context.Context, int) ([]domain.EffectiveMemoryItem, error)
}
