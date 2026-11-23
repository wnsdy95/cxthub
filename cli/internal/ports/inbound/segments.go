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
	// PushSegmentsOf uploads only the named sessions.
	PushSegmentsOf(ctx context.Context, cwd string, keys []domain.SessionUploadKey) (domain.SegmentPushResult, error)
	// ResendSession sends the copy kept under .cxt for a session again, for
	// a session the tool no longer holds. Nothing is read from the tool.
	ResendSession(ctx context.Context, cwd string, key domain.SessionUploadKey) (domain.SegmentPushResult, error)
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
	// RemoveSession removes a session, and the sessions it spawned, from the
	// server for every reader. The server remembers the removal: the session
	// is not uploaded again until it is allowed again.
	RemoveSession(ctx context.Context, cwd string, key domain.SessionUploadKey) (domain.SegmentRemoveResult, error)
	// AllowSession takes back the removal of a session and uploads the
	// session again from the tool's records.
	AllowSession(ctx context.Context, cwd string, key domain.SessionUploadKey) (domain.SegmentAllowResult, error)
	// Diagnose reads this computer and the server and says what does not
	// fit, each with the command that puts it right. It changes nothing.
	Diagnose(ctx context.Context, cwd string) (domain.SegmentDiagnosis, error)
	// RefreshRemote asks the server to read the repository from GitHub
	// again soon.
	RefreshRemote(ctx context.Context, cwd string) error
	// SetArchived archives a session on the server or restores it.
	SetArchived(ctx context.Context, cwd string, key domain.SessionUploadKey, archived bool) error
}
