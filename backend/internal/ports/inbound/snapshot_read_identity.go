package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryReadPermit interface {
	AuthorizeSnapshot(context.Context, string, string) error
}

type SnapshotReadIdentity interface {
	AdmitRepositoryRead(context.Context, string, string) (RepositoryReadPermit, error)
	ResolveUserForSnapshot(context.Context, string) (domain.User, error)
	ResolveMCPUserForSnapshot(context.Context, string, ...string) (domain.User, error)
}
