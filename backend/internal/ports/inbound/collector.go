package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// CollectorReports takes what a member's collector says about itself, and
// tells a reader of the repository where its pushes came from.
type CollectorReports interface {
	ReportCollector(ctx context.Context, repo domain.ContentHash, report domain.CollectorReport) error
	PushOrigins(ctx context.Context, repo domain.ContentHash) (domain.PushOriginView, error)
}
