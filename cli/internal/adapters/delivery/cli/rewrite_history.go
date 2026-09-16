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

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type rewriteBatch struct {
	RepoID     string            `json:"repo_id"`
	BranchID   string            `json:"branch_id"`
	WorktreeID string            `json:"worktree_id"`
	Rewrites   map[string]string `json:"rewrites"`
}

func rewriteJournalDir(worktree string) string {
	return filepath.Join(".cxt", "worktrees", worktree, "rewrite-journal")
}

// Unlike the legacy repository-wide label map, each immutable batch identifies
// the exact worktree and logical branch that observed this Git rewrite.
func recordRewriteHistory(ctx context.Context, c *Container, cwd string, rewrites map[string]string) error {
	if c.History == nil || len(rewrites) == 0 {
		return nil
	}
	p, err := c.History.CurrentPosition(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if p.WorktreeID == "" || p.BranchID == "" {
		return nil
	}
	batch := rewriteBatch{p.RepoID, p.BranchID, p.WorktreeID, rewrites}
	for old, next := range rewrites {
		if !validNonZeroGitOID(old) || !validNonZeroGitOID(next) || len(old) != len(next) || old == next {
			return fmt.Errorf("invalid full Git rewrite mapping")
		}
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%x.json", sha256.Sum256(raw))
	rel := filepath.Join(rewriteJournalDir(p.WorktreeID), name)
	root := cxtRepoRoot(ctx, cwd)
	if prior, err := providerfs.ReadRepoFile(root, rel); err == nil {
		if string(prior) != string(raw) {
			return domain.ErrHashMismatch
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return providerfs.WriteRepoFileAtomic(root, rel, raw, 0600)
}

// Replay must run even while rebase state exists. It only records observations;
// original snapshots, branch refs, and the worktree's selection stay untouched.
// Push retries a batch after an interrupted history write.
func replayRewriteHistory(ctx context.Context, c *Container, cwd string) error {
	if c.History == nil {
		return nil
	}
	p, err := c.History.CurrentPosition(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if p.WorktreeID == "" {
		return nil
	}
	root := cxtRepoRoot(ctx, cwd)
	rel := rewriteJournalDir(p.WorktreeID)
	dir, err := providerfs.EnsureRepoDir(root, rel, 0700)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var batches []rewriteBatch
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, err := providerfs.ReadRepoFile(root, filepath.Join(rel, entry.Name()))
		if err != nil {
			return err
		}
		var batch rewriteBatch
		if err := json.Unmarshal(raw, &batch); err != nil {
			return fmt.Errorf("invalid rewrite journal: %w", err)
		}
		if batch.RepoID != p.RepoID || batch.WorktreeID != p.WorktreeID || batch.BranchID == "" || entry.Name() != fmt.Sprintf("%x.json", sha256.Sum256(raw)) {
			return domain.ErrHashMismatch
		}
		batches = append(batches, batch)
	}
	if len(batches) == 0 {
		return nil
	}
	events, err := c.History.ListHistory(ctx, p.RepoID)
	if err != nil {
		return err
	}
	// Batch filenames are hashes, not clocks. Bounded passes resolve a chain
	// even when B→C sorts before A→B; replay identity prevents duplicate writes.
	for pass := 0; pass < len(batches); pass++ {
		changed := false
		for _, batch := range batches {
			observations, err := rewrittenHistory(events, batch.Rewrites, batch.BranchID, batch.WorktreeID, time.Now().UTC())
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
				events = append(events, e)
				changed = true
			}
		}
		if !changed {
			break
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
