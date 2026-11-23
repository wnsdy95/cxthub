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
// to the repository's server. On this computer it writes branch IDs into
// Git's configuration and, where cxt is set up, a copy of what it sent.
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

// SegmentCare is what a person, or the AI working for them, uses when the
// sessions on the server and on this computer do not agree: commands that
// find what is wrong and commands that put it right.
type SegmentCare interface {
	// UploadLog lists what is kept on this computer about the uploads of
	// every session, or of the one session key names.
	UploadLog(ctx context.Context, cwd string, key *domain.SessionUploadKey) ([]domain.SessionUploadListing, error)
}
