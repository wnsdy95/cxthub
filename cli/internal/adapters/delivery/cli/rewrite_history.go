package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type rewriteBatch struct {
	RepoID     string            `json:"repo_id"`
	BranchID   string            `json:"branch_id"`
	WorktreeID string            `json:"worktree_id"`
	Rewrites   map[string]string `json:"rewrites"`
	Final      bool              `json:"final,omitempty"`
}

func rewriteJournalDir(worktree string) string {
	return filepath.Join(".cxt", "worktrees", worktree, "rewrite-journal")
}

// Unlike the legacy repository-wide label map, each immutable batch identifies
// the exact worktree and logical branch that observed this Git rewrite.
func recordRewriteHistory(ctx context.Context, c *Container, cwd string, rewrites map[string]string) error {
	return recordRewriteBatch(ctx, c, cwd, rewrites, true)
}

func recordRewriteBatch(ctx context.Context, c *Container, cwd string, rewrites map[string]string, final bool) error {
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
	batch := rewriteBatch{RepoID: p.RepoID, BranchID: p.BranchID, WorktreeID: p.WorktreeID, Rewrites: rewrites, Final: final}
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
	} else if !os.IsNotExist(err) {
		return err
	}
	return providerfs.WriteRepoFileDurable(root, rel, raw, 0600)
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
	if err := replayPublications(ctx, c, cwd); err != nil {
		return err
	}
	root := cxtRepoRoot(ctx, cwd)
	dir, err := providerfs.EnsureRepoDir(root, filepath.Join(".cxt", "worktrees"), 0700)
	if err != nil {
		return err
	}
	worktrees, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var batches []rewriteBatch
	// Push from any worktree drains all journals in the shared replica. The
	// batch's immutable identity, not the caller's current branch, owns replay.
	for _, worktree := range worktrees {
		if !worktree.IsDir() {
			return fmt.Errorf("invalid rewrite worktree directory %q", worktree.Name())
		}
		rel := rewriteJournalDir(worktree.Name())
		journal, err := providerfs.EnsureRepoDir(root, rel, 0700)
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(journal)
		if err != nil {
			return err
		}
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
			if batch.RepoID != p.RepoID || batch.WorktreeID != worktree.Name() || batch.BranchID == "" || entry.Name() != fmt.Sprintf("%x.json", sha256.Sum256(raw)) {
				return domain.ErrHashMismatch
			}
			batches = append(batches, batch)
		}
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
					// Another process may have published this same semantic event
					// after our read. Adopt its original time only if every other
					// field agrees; corruption and different associations still fail.
					current, readErr := c.History.ListHistory(ctx, p.RepoID)
					if readErr != nil {
						return err
					}
					matched := false
					for _, existing := range current {
						if existing.ID != e.ID {
							continue
						}
						candidate := e
						candidate.CreatedAt = existing.CreatedAt
						if reflect.DeepEqual(candidate, existing) {
							e, matched = existing, true
						}
						break
					}
					if !matched {
						return err
					}
				}
				events = append(events, e)
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return finalizeRewritePublications(ctx, c, cwd, p.RepoID, batches)
}

func finalizeRewritePublications(ctx context.Context, c *Container, cwd, repo string, batches []rewriteBatch) error {
	events, err := c.History.ListHistory(ctx, repo)
	if err != nil {
		return err
	}
	if c.List == nil {
		return fmt.Errorf("snapshot graph unavailable for rewrite finalization")
	}
	listed, err := c.List.List(ctx, inbound.ListInput{RepoID: repo})
	if err != nil {
		return err
	}
	limit := 1
	for _, b := range batches {
		limit += len(b.Rewrites)
	}
	for pass := 0; pass < limit; pass++ {
		changed := false
		for _, b := range batches {
			// Squash fires an intermediate amend hook before the complete
			// rebase mapping. It can preserve aliases, but cannot finalize a
			// partial source even when another worktree retries this journal.
			if !b.Final {
				continue
			}
			publications, err := rewrittenPublications(events, b, listed.Snapshots)
			if err != nil {
				return err
			}
			for _, e := range publications {
				if err := persistPublication(ctx, c, cwd, e); err != nil {
					return err
				}
				changed = true
			}
			if len(publications) > 0 {
				events, err = c.History.ListHistory(ctx, repo)
				if err != nil {
					return err
				}
			}
		}
		if !changed {
			return nil
		}
	}
	return fmt.Errorf("rewrite publications did not converge")
}

// Finalization is a barrier for the whole mapping, including squash: every
// original revision must be finalized before its replacement becomes eligible.
// Legacy observations alone deliberately leave the new revision pending.
func rewrittenPublications(events []domain.HistoryEvent, b rewriteBatch, snapshots []domain.Snapshot) ([]domain.HistoryEvent, error) {
	byID := map[domain.ContentHash]domain.Snapshot{}
	for _, s := range snapshots {
		byID[s.ID] = s
	}
	contains := func(tip, ancestor domain.ContentHash) bool {
		queue := []domain.ContentHash{tip}
		seen := map[domain.ContentHash]bool{}
		for len(queue) > 0 {
			id := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if id == ancestor {
				return true
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			if s, ok := byID[id]; ok {
				queue = append(queue, s.ReachabilityParents()...)
			}
		}
		return false
	}
	pubs := map[string][]domain.HistoryEvent{}
	for _, e := range events {
		if e.Kind == "publish" && e.BranchID == b.BranchID && e.WorktreeID == b.WorktreeID {
			pubs[e.GitAfter] = append(pubs[e.GitAfter], e)
		}
	}
	inputs := map[string][]string{}
	for old, next := range b.Rewrites {
		inputs[next] = append(inputs[next], old)
	}
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []domain.HistoryEvent
	for _, next := range keys {
		if len(pubs[next]) > 0 {
			continue
		}
		var candidates []domain.HistoryEvent
		complete := true
		for _, old := range inputs[next] {
			if len(pubs[old]) == 0 {
				complete = false
				break
			}
			candidates = append(candidates, pubs[old]...)
		}
		if !complete {
			continue
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
		var winner *domain.HistoryEvent
		for i, e := range candidates {
			dominates := true
			for _, other := range candidates {
				if !contains(e.Target, other.Target) {
					dominates = false
					break
				}
			}
			if dominates {
				winner = &candidates[i]
				break
			}
		}
		if winner == nil {
			return nil, fmt.Errorf("%w: rewritten revision %s has incomparable finalized contexts", domain.ErrSyncConflict, next)
		}
		e := *winner
		e.ID, e.CreatedAt = "", time.Time{}
		e.GitBefore, e.GitAfter = winner.GitAfter, next
		// A fresh native observation can supersede the ordinary rewrite alias.
		// Do not finalize an older target whose exact alias was intentionally
		// suppressed; wait for that native capture's own completion instead.
		proven := false
		for _, proof := range events {
			if proof.Kind != "publish" && proof.Kind != "pr-merge" && proof.BranchID == e.BranchID && proof.Branch == e.Branch && proof.LocalBranch == e.LocalBranch && proof.WorktreeID == e.WorktreeID && proof.GitAfter == next && proof.Target == e.Target {
				proven = true
				break
			}
		}
		if !proven {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func rewrittenHistory(events []domain.HistoryEvent, rewrites map[string]string, branchID, worktreeID string, now time.Time) ([]domain.HistoryEvent, error) {
	known := make(map[string]bool, len(events))
	observed := make(map[string]bool)
	for _, e := range events {
		known[e.ID] = true
		if e.BranchID == branchID && e.WorktreeID == worktreeID && e.Target != "" && e.Kind != "pr-merge" && e.Kind != "publish" && e.ID != rewriteObservationID(e) {
			observed[e.GitAfter] = true
		}
	}
	var out []domain.HistoryEvent
	for _, e := range events {
		if e.Kind == "pr-merge" || e.Kind == "publish" || e.Target == "" || e.Branch == "" || e.BranchID != branchID || e.WorktreeID != worktreeID || !validNonZeroGitOID(e.GitAfter) {
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
			// A native observation at the rewritten revision already wins. In
			// particular, a delayed replay must not make an older context appear
			// newer than a fresh capture at that revision. Generated aliases do
			// not suppress sibling aliases: squash may map several conversations
			// to one Git revision, including after partially completed replay.
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
			id := rewriteObservationID(observation)
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

// Preserve the v1 semantic ID contract. This also distinguishes a generated
// alias from a fresh native observation without trusting labels or timestamps.
func rewriteObservationID(e domain.HistoryEvent) string {
	e.ID, e.CreatedAt = "", time.Time{}
	raw, _ := json.Marshal(e)
	key := sha256.Sum256(append([]byte("rewrite\x00"), raw...))
	return fmt.Sprintf("%x", key[:16])
}
