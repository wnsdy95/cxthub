package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitReadFence func(context.Context) error

type GitSourceAuthorizer interface {
	AuthorizeGitRead(context.Context, domain.ContentHash) (GitReadFence, error)
}
