package backendclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// readSnapshotMetadata acquires only the requested metadata from a freshly
// authorized manifest or branch plan. A checkpoint can avoid downloading a
// record, but the record is still returned to the caller for content validation.
// In particular it is never added to verified snapshot/doc negotiation haves.
func (c *BackendClient) readSnapshotMetadata(ctx context.Context, man domain.Manifest, wants []domain.ContentHash, remote string, complete bool) ([]domain.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.SyncRemoteIdentity() != remote {
		return nil, domain.ErrSyncConflict
	}
	// Legacy manifests have no exact metadata tokens. Retain their full reads,
	// even if another invocation has a modern checkpoint for this endpoint.
	cache := c.metadata
	if remote == "" {
		cache = nil
	}
	checkpoint := outbound.MetadataCheckpoint{Version: 1, RepoID: man.RepoID, Remote: remote}
	if cache != nil {
		var err error
		checkpoint, err = cache.ReadMetadataCheckpoint(ctx, man.RepoID, remote)
		if err != nil {
			return nil, fmt.Errorf("read metadata checkpoint: %w", err)
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
	var removed []domain.ContentHash
	if complete {
		present := setOf(man.SnapshotIndex)
		for id := range known {
			if !present[id] {
				removed = append(removed, id)
			}
		}
		sort.Slice(removed, func(i, j int) bool { return removed[i] < removed[j] })
	}
	const batchSize = 256
	save := func(snapshots []domain.Snapshot, deleted []domain.ContentHash) error {
		if cache == nil {
			return nil
		}
		if c.SyncRemoteIdentity() != remote {
			return domain.ErrSyncConflict
		}
		revision, err := cache.AppendMetadataCheckpoint(ctx, checkpoint.Revision, man.RepoID, remote, snapshots, deleted)
		if errors.Is(err, domain.ErrSyncConflict) && len(deleted) == 0 {
			// Losing an optional cache update must not abort valid acquisition.
			// Leave the winning head intact and finish using this response only.
			cache = nil
			return nil
		}
		checkpoint.Revision = revision
		if err != nil {
			return fmt.Errorf("write metadata checkpoint: %w", err)
		}
		return nil
	}
	prune := func() error {
		// A complete catalog's observed deletion must be durable before success:
		// the same ID can later be reinserted with different immutable metadata.
		for start := 0; start < len(removed); start += batchSize {
			if err := save(nil, removed[start:min(start+batchSize, len(removed))]); err != nil {
				return err
			}
		}
		removed = nil
		return nil
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
	if len(missing) == 0 {
		// GET /manifest admits viewers, whereas POST /pull/objects requires
		// pull permission. Cached metadata must not bypass that stronger gate.
		var permission pullResp
		if err := c.do(ctx, http.MethodPost, c.reposPath(man.RepoID)+"/pull/objects", pullReq{CIRVersionsSupported: domain.SupportedCIRVersions()}, &permission); err != nil {
			return nil, err
		}
		if len(permission.Snapshots)+len(permission.Docs)+len(permission.DocManifests)+len(permission.ChunkObjects) != 0 {
			return nil, domain.ErrHashMismatch
		}
		if err := prune(); err != nil {
			return nil, err
		}
	}
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
			selected[snap.ID] = snap
		}
		if len(wanted) != 0 {
			return nil, domain.ErrHashMismatch
		}
		if err := prune(); err != nil {
			return nil, err
		}
		if man.SnapshotStates != nil {
			if err := save(response.Snapshots, nil); err != nil {
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
