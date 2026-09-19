package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// SessionNoticeSelectionReader reads the exact initialized worktree and code
// HEAD, without network I/O, provider file mutation or shared-head fallback.
type SessionNoticeSelectionReader interface {
	ReadNoticeSelection(context.Context, string) (domain.SessionNoticeSelection, error)
}

// SessionNoticeCursorStore serializes delivery for one repository/worktree/
// provider/session. The returned next ID is persisted only if deliver succeeds.
// Output and durable acknowledgement are not a distributed transaction.
type SessionNoticeCursorStore interface {
	WithSessionNoticeCursor(context.Context, domain.SessionNoticeScope, func(domain.ContentHash) (domain.ContentHash, error)) error
}
