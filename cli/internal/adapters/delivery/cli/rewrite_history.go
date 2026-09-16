package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Replay the exact post-rewrite journal as ordinary code/context observations.
// This must run even while Git still has rebase state: selectCodePosition skips
// that interval. Original events, snapshots, and branch refs remain immutable.
// Push repeats this step so a failed local write does not strand PR delivery.
func replayRewriteHistory(ctx context.Context, c *Container, cwd string) error {
	if c.History == nil {
		return nil
	}
	raw, err := providerfs.ReadRepoFile(cxtRepoRoot(ctx, cwd), filepath.Join(".cxt", "rewrites.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var rewrites map[string]string
	if err := json.Unmarshal(raw, &rewrites); err != nil {
		return fmt.Errorf("invalid rewrite journal: %w", err)
	}
	repo, err := remotecfg.Wrap(cwd, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
	if err != nil {
		return err
	}
	events, err := c.History.ListHistory(ctx, repo.ID)
	if err != nil {
		return err
	}
	position, err := c.History.CurrentPosition(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if position.RepoID != repo.ID || position.WorktreeID == "" || position.BranchID == "" {
		return nil
	}
	// Only exact object identities from Git's journal are evidence. A global
	// rewrite map is not a reason to substitute today's branch tip or reuse a
	// released name: each event retains its original logical branch identity.
	observations, err := rewrittenHistory(events, rewrites, position.BranchID, position.WorktreeID, time.Now().UTC())
	if err != nil {
		return err
	}
	for _, e := range observations {
		if _, err := c.History.ValidateHistorySource(ctx, e); err != nil {
			return err
		}
		if err := c.History.RecordHistory(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func rewrittenHistory(events []domain.HistoryEvent, rewrites map[string]string, branchID, worktreeID string, now time.Time) ([]domain.HistoryEvent, error) {
	known := make(map[string]bool, len(events))
	observed := make(map[string]bool)
	for _, e := range events {
		known[e.ID] = true
		if e.BranchID == branchID && e.WorktreeID == worktreeID && e.Target != "" && e.Kind != "pr-merge" {
			observed[e.GitAfter] = true
		}
	}
	var out []domain.HistoryEvent
	for _, e := range events {
		if e.Kind == "pr-merge" || e.Target == "" || e.Branch == "" || e.BranchID != branchID || e.WorktreeID != worktreeID || !validNonZeroGitOID(e.GitAfter) {
			continue
		}
		old := e.GitAfter
		seen := map[string]bool{old: true}
		for {
			next, ok := rewrites[old]
			if !ok {
				break
			}
			if !validNonZeroGitOID(next) || len(next) != len(old) || seen[next] {
				return nil, fmt.Errorf("invalid or cyclic Git rewrite at %s", old)
			}
			seen[next] = true
			// An exact observation at the rewritten revision already wins. In
			// particular, a delayed replay must not make an older context appear
			// newer than a fresh capture at that revision. Chained replay starts
			// from that observation in its own iteration.
			if observed[next] {
				break
			}
			observation := domain.HistoryEvent{
				RepoID: e.RepoID, BranchID: e.BranchID, Branch: e.Branch,
				LocalBranch: e.LocalBranch, WorktreeID: e.WorktreeID, Kind: "position",
				Source: e.Target, Target: e.Target, MemoryHash: e.MemoryHash,
				MemorySource: e.MemorySource, MemoryPinned: e.MemoryPinned,
				GitBefore: old, GitAfter: next,
			}
			// Semantic identity, independent of the source event and retry time,
			// keeps chained rewrites idempotent even when aliases are replayed.
			raw, _ := json.Marshal(observation)
			key := sha256.Sum256(append([]byte("rewrite\x00"), raw...))
			id := fmt.Sprintf("%x", key[:16])
			if !known[id] {
				observation.ID, observation.CreatedAt = id, now
				out = append(out, observation)
				known[id] = true
			}
			old = next
		}
	}
	return out, nil
}
