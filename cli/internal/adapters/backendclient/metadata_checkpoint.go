package backendclient

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// readSnapshotMetadata acquires only the requested metadata from a freshly
// authorized manifest or branch plan. A checkpoint can avoid downloading a
// record, but the record is still returned to the caller for content validation.
// In particular it is never added to verified snapshot/doc negotiation haves.
func (c *BackendClient) readSnapshotMetadata(ctx context.Context, man domain.Manifest, wants []domain.ContentHash, remote string) ([]domain.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.SyncRemoteIdentity() != remote {
		return nil, domain.ErrSyncConflict
	}
	if len(wants) == 0 {
		return nil, nil
	}
	// Legacy manifests have no exact metadata tokens. Retain their full reads,
	// even if another invocation has a modern checkpoint for this endpoint.
	cache := c.metadata
	if man.SnapshotStates == nil || remote == "" {
		cache = nil
	}
	checkpoint := outbound.MetadataCheckpoint{Version: 1, RepoID: man.RepoID, Remote: remote}
	if cache != nil {
		var err error
		checkpoint, err = cache.ReadMetadataCheckpoint(ctx, man.RepoID, remote)
		if err != nil {
			return nil, err
		}
		if checkpoint.Version != 1 || checkpoint.RepoID != man.RepoID || checkpoint.Remote != remote {
			return nil, domain.ErrHashMismatch
		}
	}
	known := make(map[domain.ContentHash]domain.Snapshot, len(checkpoint.Snapshots)+len(wants))
	for _, snap := range checkpoint.Snapshots {
		if err := validateSnapshotObject(snap); err != nil {
			return nil, err
		}
		if _, duplicate := known[snap.ID]; duplicate || snap.RepoID != man.RepoID {
			return nil, domain.ErrHashMismatch
		}
		known[snap.ID] = snap
	}
	selected := make(map[domain.ContentHash]domain.Snapshot, len(wants))
	seen := make(map[domain.ContentHash]bool, len(wants))
	var missing []domain.ContentHash
	for _, id := range wants {
		if domain.ValidateContentHash(id) != nil || seen[id] {
			return nil, domain.ErrHashMismatch
		}
		seen[id] = true
		if man.SnapshotStates != nil && domain.ValidateContentHash(man.SnapshotStates[id]) != nil {
			return nil, domain.ErrHashMismatch
		}
		if snap, ok := known[id]; ok {
			state, err := domain.SnapshotStateHash(snap)
			if err == nil && state == man.SnapshotStates[id] {
				selected[id] = snap
				continue
			}
		}
		missing = append(missing, id)
	}
	const batchSize = 256
	for start := 0; start < len(missing); start += batchSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if c.SyncRemoteIdentity() != remote {
			return nil, domain.ErrSyncConflict
		}
		ids := missing[start:min(start+batchSize, len(missing))]
		var response pullResp
		if err := c.do(ctx, http.MethodPost, c.reposPath(man.RepoID)+"/pull/objects", pullReq{SnapshotWants: ids, CIRVersionsSupported: domain.SupportedCIRVersions()}, &response); err != nil {
			return nil, err
		}
		if c.SyncRemoteIdentity() != remote {
			return nil, domain.ErrSyncConflict
		}
		if len(response.Docs)+len(response.DocManifests)+len(response.ChunkObjects) != 0 {
			return nil, domain.ErrHashMismatch
		}
		wanted := setOf(ids)
		for _, snap := range response.Snapshots {
			if err := validateSnapshotObject(snap); err != nil {
				return nil, err
			}
			if snap.RepoID != man.RepoID || !wanted[snap.ID] {
				return nil, domain.ErrHashMismatch
			}
			delete(wanted, snap.ID)
			if man.SnapshotStates != nil {
				state, err := domain.SnapshotStateHash(snap)
				if err != nil || state != man.SnapshotStates[snap.ID] {
					return nil, domain.ErrHashMismatch
				}
			}
			selected[snap.ID], known[snap.ID] = snap, snap
		}
		if len(wanted) != 0 {
			return nil, domain.ErrHashMismatch
		}
		if cache != nil {
			checkpoint.Snapshots = make([]domain.Snapshot, 0, len(known))
			for _, snap := range known {
				checkpoint.Snapshots = append(checkpoint.Snapshots, snap)
			}
			var err error
			checkpoint, err = cache.CompareAndSwapMetadataCheckpoint(ctx, checkpoint.Revision, checkpoint)
			if err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.SyncRemoteIdentity() != remote {
		return nil, domain.ErrSyncConflict
	}
	out := make([]domain.Snapshot, 0, len(wants))
	for _, id := range wants {
		out = append(out, selected[id])
	}
	return out, nil
}
