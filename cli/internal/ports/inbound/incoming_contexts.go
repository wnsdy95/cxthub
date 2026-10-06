package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// IncomingContextDiscovery supplies newest-first local and remote candidate
// labels without downloading bodies or publishing an observation. These labels
// are not remote graph proof: callers must fetch selected dependencies before
// promotion. False support retains the older complete-transfer path.
type IncomingContextDiscovery interface {
	DiscoverIncomingSnapshots(context.Context, SyncInput) ([]domain.Snapshot, bool, error)
}
