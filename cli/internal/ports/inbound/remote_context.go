package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type RemoteBranchContextReader interface {
	ResolveRemoteBranchContext(context.Context, SyncInput, string) (domain.ContextQueryView, error)
}
