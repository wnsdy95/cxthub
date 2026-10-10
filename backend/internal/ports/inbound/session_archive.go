package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type SessionArchiver interface {
	SetSessionArchived(context.Context, domain.ContentHash, domain.ContentHash, bool) error
}
