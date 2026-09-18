package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Read-only evidence, scoped to repository and bound Git origin. Writes are
// published by the fenced observation transaction, never by query adapters.
type GitTreeStore interface {
	GetGitCommitTree(context.Context, domain.ContentHash, string, string) (domain.GitCommitTree, error)
	GetGitTreeNode(context.Context, domain.ContentHash, string, string) (domain.GitTreeNode, error)
}
type GitTreeReader interface {
	ReadCommitTree(context.Context, string, string) (domain.GitTreeEvidence, error)
}

// GitDeltaReader is the read side of discovery storage; query services cannot
// enqueue, claim or finish provider work through this port.
type GitDeltaReader interface {
	GetGitDelta(context.Context, domain.ContentHash, string, string, string) (domain.GitCommitDelta, error)
}
