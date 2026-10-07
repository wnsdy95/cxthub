package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// MetadataCheckpoint records fetched snapshot metadata for one repository and
// endpoint. It does not attest that documents or attachments were verified, or
// that any snapshot, ref, history, or worktree state was applied locally.
type MetadataCheckpoint struct {
	Version   int                `json:"version"`
	RepoID    string             `json:"repo_id"`
	Remote    string             `json:"remote"`
	Revision  domain.ContentHash `json:"revision"`
	Snapshots []domain.Snapshot  `json:"snapshots"`
}

// MetadataCheckpointStore keeps fetched metadata separate from verified remote
// observations. Remote is an endpoint identity without credentials or tokens.
type MetadataCheckpointStore interface {
	// ReadMetadataCheckpoint returns version 1 with an empty revision when absent.
	// Corrupt records are errors and must not be silently replaced.
	ReadMetadataCheckpoint(ctx context.Context, repo, remote string) (MetadataCheckpoint, error)
	// CompareAndSwapMetadataCheckpoint returns the persisted checkpoint with its
	// checksum revision, replacing next.Revision. Expected must match the stored
	// revision, or be empty for an absent checkpoint. A mismatch is ErrSyncConflict.
	// Snapshots are sorted by ID without reordering the caller's slice. Nil and
	// empty snapshot lists are equivalent. The checksum covers the canonical JSON
	// payload (version, repo_id, remote, snapshots), excluding revision entirely.
	// Other snapshot fields, including parent order, retain their JSON semantics.
	CompareAndSwapMetadataCheckpoint(ctx context.Context, expected domain.ContentHash, next MetadataCheckpoint) (MetadataCheckpoint, error)
}
