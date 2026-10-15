package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type UnsyncDeletionStore interface {
	DeleteUnsyncIfPresent(context.Context, domain.ContentHash, string, string) (bool, error)
}
