package backendclient

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ outbound.RemoteSnapshotCatalog = (*BackendClient)(nil)

// ReadSnapshotCatalog deliberately does not call pullCatalog: candidate labels
// do not need documents, chunks or attachments. Its optional checkpoint is
// acquisition progress only; selected fetch still verifies all dependencies.
func (c *BackendClient) ReadSnapshotCatalog(ctx context.Context, repo string) ([]domain.Snapshot, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return nil, err
	}
	_, snapshots, supported, err := c.acquireCatalog(ctx, repo)
	if err != nil || supported {
		return snapshots, err
	}
	remote := c.SyncRemoteIdentity()
	man, err := c.RemoteManifest(ctx, repo)
	if err != nil {
		return nil, err
	}
	if man.SnapshotStates == nil && len(man.SnapshotIndex) != 0 {
		return nil, domain.ErrHashMismatch
	}
	return c.readSnapshotMetadata(ctx, man, man.SnapshotIndex, remote, true)
}
