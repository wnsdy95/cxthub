package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func resolvePublicationRepo(ctx context.Context, c *Container, cwd string) (domain.Repo, error) {
	if c.ResolveRepo != nil {
		return c.ResolveRepo(ctx, cwd)
	}
	return remotecfg.Wrap(cwd, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
}

// Immutable local history is the durable outbox. Successful exact remote
// acceptance is its acknowledgement; no second queue or current-tip push is
// needed. A blocked identity does not starve another independent identity.
func runBranchHistorySync(ctx context.Context, c *Container, cwd string) error {
	if c.History == nil || c.Sync == nil {
		return fmt.Errorf("branch history publication service unavailable")
	}
	repo, err := resolvePublicationRepo(ctx, c, cwd)
	if err != nil {
		return err
	}
	history, err := c.History.ListHistory(ctx, repo.ID)
	if err != nil {
		return err
	}
	branches, err := domain.PublicationHistoryBranches(repo.ID, history)
	if err != nil {
		return err
	}
	remaining := branches
	for len(remaining) > 0 {
		var pending []domain.PublicationBranch
		var failures []error
		for i, branch := range remaining {
			if err := ctx.Err(); err != nil {
				return err
			}
			attempt, cancel := context.WithCancel(ctx)
			if deadline, ok := ctx.Deadline(); ok {
				cancel()
				// Reserve a share for each later identity under the same parent
				// deadline. A slow first request must not win every future wake.
				attempt, cancel = context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(remaining)-i))
			}
			err := replayRewriteHistoryForBranches(attempt, c, cwd, map[string]bool{branch.BranchID: true})
			if err == nil {
				scope := domain.PublicationScope{Branches: []domain.PublicationBranch{branch}, HistoryOnly: true}
				_, err = c.Sync.Push(attempt, inbound.SyncInput{RepoID: repo.ID, Cwd: cwd, ForegroundOnly: true, Publication: &scope})
			}
			cancel()
			if err != nil {
				pending = append(pending, branch)
				failures = append(failures, fmt.Errorf("branch %q: %w", branch.Branch, err))
			}
		}
		if len(pending) == len(remaining) {
			return errors.Join(failures...)
		}
		// Retry only after at least one identity completed; the number of
		// remaining identities strictly decreases and the hook deadline holds.
		remaining = pending
	}
	return nil
}
