package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// RemoteSnapshotCatalog reads only metadata for candidate discovery. It must
// check the catalog's exact snapshot tokens; it grants no body/graph proof and
// must not populate a verified RemoteObservation.
type RemoteSnapshotCatalog interface {
	ReadSnapshotCatalog(context.Context, string) ([]domain.Snapshot, error)
}
