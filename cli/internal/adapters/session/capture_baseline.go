package session

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

// Keep the capture exclusion with native session creation, for every caller.
// Ledger errors retain the existing best-effort materialization contract.
func recordMaterialized(ctx context.Context, cwd, path string) {
	root := cwd
	if owner, ok := gitctx.ContextRoot(ctx, cwd); ok {
		root = owner
	}
	_ = providerfs.RecordMaterialized(root, path)
}
