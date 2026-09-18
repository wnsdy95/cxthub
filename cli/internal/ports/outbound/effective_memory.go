package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// EffectiveMemoryReader has no local mutation or remote publication capability.
// Only the cloud application service assesses current code applicability.
type EffectiveMemoryReader interface {
	QueryEffectiveMemory(context.Context, string, domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error)
}

// CodePosition reads the actual target worktree, including detached HEAD.
type CodePosition interface {
	CurrentCommit(context.Context, string) (string, error)
}
