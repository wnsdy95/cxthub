// githook.go — event handlers called by git hooks (scripts targeted by githooks adapters).
//
// "Using git automatically follows cxt":
//
//	git commit          → post-commit   → active session snapshot (commit message + SHA link)
//	git checkout -b X   → post-checkout → fork current context to X
//	git checkout X      → post-checkout → restore context X (auto) or prompt (prepare)
//	git push            → pre-push      → cxt push (when origin is registered)
//	git pull/merge      → post-merge    → cxt pull (when origin is registered)
//
// All handlers are fail-open: failures are logged as stderr warnings and return nil,
// never blocking git operations.
package cli

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/githooks"
)

func cxtRepoRoot(ctx context.Context, cwd string) string {
	if root, ok := gitctx.ContextRoot(ctx, cwd); ok {
		return root
	}
	return cwd
}

// runGitHook handles `cxt git-hook <name>`. A Git hook only wakes the session
// collector, and `segment-collect` is the collector itself. Any other name is
// treated as a wake: a hook script an earlier version installed still calls
// names that no longer exist, and it must neither fail nor stop Git.
func runGitHook(ctx context.Context, c *Container, cwd string, rest []string) error {
	if len(rest) > 0 && rest[0] == "segment-collect" {
		return runSegmentCollector(ctx, c, cwd)
	}
	state := gitctx.InspectContextRoot(ctx, cwd)
	// A .cxt directory is kept out of Git whether or not cxt was set up in it.
	if state.GitRepository && state.Exists {
		_ = githooks.EnsureIgnored(state.Root)
	}
	if !state.GitRepository || !state.Initialized {
		return nil
	}
	// Scripts of an earlier version are replaced by the ones that only wake.
	_ = githooks.Upgrade(state.WorktreeRoot)
	if c != nil && c.WakeSegmentCollector != nil {
		c.WakeSegmentCollector(cwd)
	}
	return nil
}

// --- History Rewrite Mapping (.cxt/rewrites.json) ---
// Snapshot message [git <sha>] links are immutable (content-addressed), so they cannot be modified.
// Similar to git's refs/replace, we accumulate old→new commit mappings in a side table and follow the chain when interpreting links (aaa→bbb→ccc).
