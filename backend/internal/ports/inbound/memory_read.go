package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type MemoryReadScope interface {
	WithMemoryReadResources(context.Context, domain.ContentHash, func(context.Context) error) error
}
