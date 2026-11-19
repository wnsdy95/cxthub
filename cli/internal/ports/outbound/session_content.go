package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// SessionContentSource returns to where a tool keeps a session's records and
// reads the conversation of the named records. It applies the repository's
// masking policy exactly as capture does. It reads only.
type SessionContentSource interface {
	ReadSessionContent(ctx context.Context, repoRoot string, records domain.SessionRecordRanges) (domain.SessionContent, error)
}

// SessionContentRemote stores a session's content document on the server and
// returns the reference the server knows it by. Only chunks the server does
// not have yet are sent. It publishes no snapshot, ref or pending context.
type SessionContentRemote interface {
	PutSessionContent(ctx context.Context, repoID string, doc domain.CIRDocument) (domain.DocumentRef, error)
}
