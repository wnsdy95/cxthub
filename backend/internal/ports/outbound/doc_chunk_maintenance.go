package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// DocChunkMaintenance removes document chunks that no stored manifest and no
// pending finalization job of their repository names any more. Each call
// visits at most one repository and never deletes a chunk before its grace
// period has passed, so it is safe beside live uploads. A result without a
// repository means there is nothing to visit for now.
type DocChunkMaintenance interface {
	SweepNextDocChunks(context.Context) (domain.DocChunkSweep, error)
}
