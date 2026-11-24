// githook.go — what the Git hooks cxt installs run.
//
// The five hooks (post-commit, post-checkout, post-merge, post-rewrite,
// pre-push) do one thing: they wake the collector that reads the agent tools'
// session records and Git. A hook reads nothing itself and decides nothing,
// so a hook that did not run is made up for by the next wake.
//
// All handlers are fail-open: a failure is a warning on stderr and nil is
// returned, so a Git command is never blocked.
package cli

import (
	"context"
	"slices"

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
		return runSegmentCollector(ctx, c, cwd, len(rest) > 1 && rest[1] == segmentCollectAfterPush)
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
	if c == nil {
		return nil
	}
	// A hook script names itself after the word `wake`. Git runs pre-push
	// before the push and has no hook after it.
	if slices.Contains(rest, "pre-push") && c.WakeSegmentCollectorAfterPush != nil {
		c.WakeSegmentCollectorAfterPush(cwd)
		return nil
	}
	if c.WakeSegmentCollector != nil {
		c.WakeSegmentCollector(cwd)
	}
	return nil
}
