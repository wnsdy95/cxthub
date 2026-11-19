package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// SessionRecordSource reads the sessions one coding agent recorded for a
// repository. A record that exists but cannot be read is returned as an unread
// record, never dropped; an error is returned only when reading was cancelled.
type SessionRecordSource interface {
	ReadSessionRecords(ctx context.Context, scope domain.SessionRecordScope) (domain.SessionRecordBatch, error)
}

// GitRecordSource reads what Git recorded about the repository holding cwd.
type GitRecordSource interface {
	ReadGitRecords(ctx context.Context, cwd string) (domain.GitRecords, []domain.UnreadSessionRecord, error)
}
