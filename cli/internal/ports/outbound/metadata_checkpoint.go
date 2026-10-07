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
	// AppendMetadataCheckpoint atomically publishes 1..256 snapshots/removals and
	// returns the new head checksum. Expected must match the head revision, or be
	// empty when absent; a mismatch is ErrSyncConflict. Duplicate IDs within a
	// batch or across snapshots/removals are invalid. Later batches replace or
	// delete earlier metadata for the same ID. Removals require an authoritative
	// full manifest; a partial branch plan must never prune retained metadata.
	// The head checksum binds a monotonic generation so compaction cannot revive
	// a stale revision when it produces the same live page list again.
	// Immutable pages are sorted by ID without changing the caller's slice; all
	// other fields, including parent order, retain their JSON semantics.
	// Ordinary appends validate only the head and the new page. Full reads and
	// periodic compaction verify all referenced pages from their current bytes.
	// Unreferenced pages are retained; publication failures may leave safe orphans.
	AppendMetadataCheckpoint(ctx context.Context, expected domain.ContentHash, repo, remote string, snapshots []domain.Snapshot, removed []domain.ContentHash) (domain.ContentHash, error)
}
