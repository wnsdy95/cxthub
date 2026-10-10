package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type SessionArchiveStore interface {
	ListSessionArchives(context.Context, domain.ContentHash) ([]domain.SessionArchive, error)
	PutSessionArchive(context.Context, domain.SessionArchive) error
	DeleteSessionArchive(context.Context, domain.ContentHash, domain.ContentHash) error
}
