// githook.go — what the Git hooks cxt installs run.
//
// The five hooks (post-commit, post-checkout, post-merge, post-rewrite,
// pre-push) do one thing: they wake the collector that reads the agent tools'
// session records and Git. A hook reads nothing itself and decides nothing,
// so a hook that did not run is made up for by the next wake.
//
// Git runs pre-push before the push and has no hook after it. What a push
// changes is there to read when the Git process that pushes has ended, so
// that hook's wake is held until then.
//
// All handlers are fail-open: a failure is a warning on stderr and nil is
// returned, so a Git command is never blocked.
package cli

import (
	"context"
	"strconv"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/githooks"
)

func cxtRepoRoot(ctx context.Context, cwd string) string {
	if root, ok := gitctx.ContextRoot(ctx, cwd); ok {
		return root
	}
	return cwd
}

// pushingProcess reads what the pre-push hook script says of itself: `wake
// pre-push <the process that runs the hook>`. Any other words, those of
// another hook or of a script an earlier version installed, name no push.
func pushingProcess(rest []string) (int, bool) {
	if len(rest) != 3 || rest[0] != "wake" || rest[1] != "pre-push" {
		return 0, false
	}
	// The first process of a system is no Git, and nothing below it is a process.
	process, err := strconv.Atoi(rest[2])
	return process, err == nil && process > 1
}

// runGitHook handles `cxt git-hook <name>`. A Git hook only wakes the session
// collector; `segment-collect` is the collector itself and `after-push` the
// wait for a push to end. Any other name is treated as a wake: a hook script
// an earlier version installed still calls names that no longer exist, and it
// must neither fail nor stop Git.
func runGitHook(ctx context.Context, c *Container, cwd string, rest []string) error {
	if len(rest) > 0 && rest[0] == "segment-collect" {
		return runSegmentCollector(ctx, c, cwd)
	}
	if len(rest) == 2 && rest[0] == wakeAfterPush {
		return runWakeAfterPush(ctx, c, cwd, rest[1])
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
	// The collector a push wakes runs when the push has ended, and not before:
	// what the hook's own moment holds was read at the commit.
	if pushing, ok := pushingProcess(rest); ok && c.WakeSegmentCollectorAfterPush != nil {
		c.WakeSegmentCollectorAfterPush(cwd, pushing)
		return nil
	}
	if c.WakeSegmentCollector != nil {
		c.WakeSegmentCollector(cwd)
	}
	return nil
}
