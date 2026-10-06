package backendclient

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ outbound.RemoteSnapshotCatalog = (*BackendClient)(nil)

// ReadSnapshotCatalog deliberately does not call pullCatalog: candidate labels
// do not need documents, chunks or attachments. Metadata is transient until a
// separate selected fetch verifies its dependencies under fresh authorization.
func (c *BackendClient) ReadSnapshotCatalog(ctx context.Context, repo string) ([]domain.Snapshot, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return nil, err
	}
	man, err := c.RemoteManifest(ctx, repo)
	if err != nil {
		return nil, err
	}
	if man.SnapshotStates == nil && len(man.SnapshotIndex) != 0 {
		return nil, domain.ErrHashMismatch
	}
	var out []domain.Snapshot
	const batchSize = 256
	for start := 0; start < len(man.SnapshotIndex); start += batchSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ids := man.SnapshotIndex[start:min(start+batchSize, len(man.SnapshotIndex))]
		var response pullResp
		if err := c.do(ctx, http.MethodPost, c.reposPath(repo)+"/pull/objects", pullReq{SnapshotWants: ids, CIRVersionsSupported: domain.SupportedCIRVersions()}, &response); err != nil {
			return nil, err
		}
		if len(response.Docs)+len(response.DocManifests)+len(response.ChunkObjects) != 0 {
			return nil, domain.ErrHashMismatch
		}
		wanted := setOf(ids)
		seen := make(map[domain.ContentHash]bool, len(ids))
		for _, snap := range response.Snapshots {
			if err := validateSnapshotObject(snap); err != nil {
				return nil, err
			}
			if snap.RepoID != repo || !wanted[snap.ID] || seen[snap.ID] {
				return nil, domain.ErrHashMismatch
			}
			state, err := domain.SnapshotStateHash(snap)
			if err != nil || state != man.SnapshotStates[snap.ID] {
				return nil, domain.ErrHashMismatch
			}
			seen[snap.ID] = true
			out = append(out, snap)
		}
		if len(seen) != len(ids) {
			return nil, domain.ErrHashMismatch
		}
	}
	return out, ctx.Err()
}
