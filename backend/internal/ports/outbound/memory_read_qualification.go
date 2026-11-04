package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type MemoryReadQualificationReader interface {
	WithQualifiedMemoryReadRange(context.Context, domain.ContentHash, domain.ContentHash, func(context.Context) (context.Context, error), func(context.Context, domain.MemoryReadHeader, MemoryReadChunkReader) error) error
}

type MemoryReadQualificationStore interface {
	MemoryReadQualificationReader
	QualifyMemoryRead(context.Context, domain.MemoryReadQualification) (domain.ContentHash, error)
}
