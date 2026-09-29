package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// RemoteObservation is verified remote metadata, separate from locally applied
// snapshots, branch identities and worktree memory. Revision is a local CAS
// token, not a claim about a server database transaction revision.
type RemoteObservation struct {
	Version   int                   `json:"version"`
	RepoID    string                `json:"repo_id"`
	Remote    string                `json:"remote"`
	Revision  domain.ContentHash    `json:"revision"`
	Snapshots []domain.Snapshot     `json:"snapshots"`
	Refs      []domain.Ref          `json:"refs"`
	History   []domain.HistoryEvent `json:"history"`
}

type RemoteObservationStore interface {
	ReadRemoteObservation(context.Context, string, string) (RemoteObservation, error)
	CompareAndSwapRemoteObservation(context.Context, domain.ContentHash, RemoteObservation) error
}

// SyncRemoteIdentity scopes disposable observations when two endpoints expose
// the same repository ID. It must not contain credentials or access tokens.
type SyncRemoteIdentity interface{ SyncRemoteIdentity() string }
