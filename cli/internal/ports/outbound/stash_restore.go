package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// StashRestoreStore acknowledges only the exact stack inspected before
// preparation. Provider preparation is outside the short mutation lock.
type StashRestoreStore interface {
	CompareAndDropStash(context.Context, string, []domain.StashEntry) error
}
