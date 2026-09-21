package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitSyncAudit interface {
	CheckGitHubSync(context.Context, domain.ContentHash, string) (domain.SyncAuditPage, error)
}
