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
	// KeepSessionUpload stores the report, stores the conversation when one
	// is given and differs from the copy kept, and appends entry to the log
	// when it is eventful, when a file changed, or when nothing was logged
	// yet, unless it only repeats the last line.
	KeepSessionUpload(ctx context.Context, cwd string, upload domain.AnchoredSessionUpload, conversation *domain.CIRDocument, entry domain.SessionUploadLogEntry) error
	// LogSessionAction appends a line that is not an upload.
	LogSessionAction(ctx context.Context, cwd string, key domain.SessionUploadKey, entry domain.SessionUploadLogEntry) error
	// ListSessionUploads names every session a record is kept for.
	ListSessionUploads(ctx context.Context, cwd string) ([]domain.SessionUploadKey, error)
	// ReadSessionUpload reads one session's record. withConversation also
	// reads the conversation document, which can be large.
	ReadSessionUpload(ctx context.Context, cwd string, key domain.SessionUploadKey, withConversation bool) (domain.SessionUploadRecord, error)
}
