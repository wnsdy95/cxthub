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

// SegmentUploader computes the same report and uploads one report per session
// to the repository's server. It writes nothing on this computer.
type SegmentUploader interface {
	PushSegments(ctx context.Context, cwd string) (domain.SegmentPushResult, error)
}

// SegmentCollector is the upload nobody has to ask for. It reads the same
// records by the same rules whatever woke it, leaves out sessions from before
// automatic reading began in the repository, and sends only what changed
// since its last run.
type SegmentCollector interface {
	CollectSegments(ctx context.Context, cwd string) (domain.SegmentPushResult, error)
}
