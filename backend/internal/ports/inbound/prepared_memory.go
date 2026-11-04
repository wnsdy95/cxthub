package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type PreparedMemoryQuery interface {
	QueryPreparedMemory(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error)
}

type PreparedMemoryReader interface {
	PreparedMemoryQuery
	MemoryReadScope
}

type PreparedMemoryDemand interface {
	RequestPreparedMemory(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error
}
