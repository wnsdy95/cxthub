package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type MemoryPreparationRequester interface {
	RequestMemoryPreparation(context.Context, string, domain.MemoryReadPreparationRequest) error
}
