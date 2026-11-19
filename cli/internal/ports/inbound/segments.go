package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// SegmentReporter reads the session records and Git records of the repository
// holding cwd and reports sessions cut at their commits. It writes nothing.
type SegmentReporter interface {
	SegmentReport(ctx context.Context, cwd string) (domain.SegmentReport, error)
}
