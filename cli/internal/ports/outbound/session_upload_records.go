package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// SessionUploadRecords keeps, in the repository's .cxt folder on this
// computer, what was uploaded for each session: the report as it was sent,
// the conversation document as it was sent, and a log of the uploads. They
// are a record to compare the server with and to send again from; no upload
// is computed from them.
type SessionUploadRecords interface {
	// KeepSessionUpload stores the report, stores each document of the
	// conversation that was built in this run and is not kept yet, removes the
	// documents the stored report no longer names, and appends entry to the
	// log when it is eventful, when a file changed, or when nothing was logged
	// yet, unless it only repeats the last line.
	KeepSessionUpload(ctx context.Context, cwd string, upload domain.AnchoredSessionUpload, built []domain.SessionPartDocument, entry domain.SessionUploadLogEntry) error
	// LogSessionAction appends a line that is not an upload, unless it only
	// repeats the last line.
	LogSessionAction(ctx context.Context, cwd string, key domain.SessionUploadKey, entry domain.SessionUploadLogEntry) error
	// ListSessionUploads names every session a record is kept for.
	ListSessionUploads(ctx context.Context, cwd string) ([]domain.SessionUploadKey, error)
	// ReadSessionUpload reads one session's record. withConversation also
	// reads the documents of the conversation, which can be large.
	ReadSessionUpload(ctx context.Context, cwd string, key domain.SessionUploadKey, withConversation bool) (domain.SessionUploadRecord, error)
}
