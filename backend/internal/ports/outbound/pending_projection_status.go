package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type PendingProjectionStatusStore interface {
	ReadPendingProjectionStatus(context.Context, domain.ContentHash) (domain.PendingProjectionRead, error)
}
