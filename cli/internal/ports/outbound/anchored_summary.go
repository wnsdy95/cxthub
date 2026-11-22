package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// SessionSummarizer writes a session down at points of its conversation, the
// way a capture's memory is made.
type SessionSummarizer interface {
	// Summarize returns one digest for each end. conversation holds the
	// session's content events from the start, already masked; ends[i] is how
	// many of them make the conversation up to point i. folder is where the
	// session started; the tool's own memory is read for it once, as it is
	// now, and left out when it cannot be read.
	Summarize(ctx context.Context, folder string, provider domain.ProviderKind, sessionID string, conversation domain.CIRDocument, ends []int) ([]domain.MemoryDigest, error)
}

// AnchoredSummaryRemote stores the summary of a session at a commit a
// segment of its report ends at. The server takes the uploader from the
// signed-in account; the report must be stored first.
type AnchoredSummaryRemote interface {
	PutAnchoredSummary(ctx context.Context, repoID string, provider domain.ProviderKind, sessionID, commit string, body domain.AnchoredSummaryBody) error
}
