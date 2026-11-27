package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// ViewRevisions answers the mark an open page of a repository follows to
// know when to read its lists again.
type ViewRevisions interface {
	// ViewRevision returns a mark that is another one after anything the
	// Sessions tab shows was stored: a session's report, summary, archive or
	// removal, a note, a worktree list, a branch registration, a push the
	// Git host delivered, and the end of a read of the Git host. Two marks
	// can be compared for being the same and for nothing else.
	ViewRevision(ctx context.Context, repo domain.ContentHash) (string, error)
}
