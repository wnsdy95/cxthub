package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"
)

type prDiscovery struct {
	Origin    string    `json:"origin"`
	Branch    string    `json:"branch"`
	SHAs      []string  `json:"shas"`
	CreatedAt time.Time `json:"created_at"`
}

// persistPRDiscovery records the Git range before any remote IO can consume the
// hook deadline. The next merge may overwrite ORIG_HEAD even when fetch fails.
func persistPRDiscovery(ctx context.Context, cwd, branch, origin string, shas []string) error {
	if branch == "" || branch == "HEAD" || origin == "" || len(shas) == 0 {
		return nil
	}
	root := cxtRepoRoot(ctx, cwd)
	rel := filepath.Join(".cxt", "pr-discovery")
	for start := 0; start < len(shas); start += 200 {
		chunk := shas[start:min(start+200, len(shas))]
		identity, _ := json.Marshal([]any{origin, branch, chunk})
		key := fmt.Sprintf("%x", sha256.Sum256(identity))
		file := filepath.Join(rel, key+".json")
		if raw, err := providerfs.ReadRepoFile(root, file); err == nil {
			var saved prDiscovery
			if json.Unmarshal(raw, &saved) != nil || saved.Origin != origin || saved.Branch != branch || !slices.Equal(saved.SHAs, chunk) {
				return fmt.Errorf("persist PR discovery: existing range %s is corrupt: %w", key, domain.ErrHashMismatch)
			}
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("persist PR discovery: %w", err)
		}
		raw, _ := json.Marshal(prDiscovery{Origin: origin, Branch: branch, SHAs: chunk, CreatedAt: time.Now().UTC()})
		if err := providerfs.WriteRepoFileAtomic(root, file, raw, 0o600); err != nil {
			return fmt.Errorf("persist PR discovery: %w", err)
		}
	}
	return nil
}

func replayPRDiscovery(ctx context.Context, resolver outbound.PullRequestMergeResolver, syncer mergedPRContextSync, cwd, branch, origin string, shas []string) bool {
	if resolver == nil || syncer == nil || branch == "" || branch == "HEAD" || origin == "" {
		return false
	}
	if err := persistPRDiscovery(ctx, cwd, branch, origin, shas); err != nil {
		hookWarn("%v", err)
		return false
	}
	root := cxtRepoRoot(ctx, cwd)
	rel := filepath.Join(".cxt", "pr-discovery")
	// PrepareRepoFile validates every parent before listing the directory.
	probe, err := providerfs.PrepareRepoFile(root, filepath.Join(rel, "probe"), 0o700)
	if err != nil {
		hookWarn("PR discovery queue unavailable: %v", err)
		return false
	}
	entries, err := os.ReadDir(filepath.Dir(probe))
	if err != nil {
		hookWarn("PR discovery queue unavailable: %v", err)
		return false
	}
	type pending struct {
		path string
		item prDiscovery
	}
	var work []pending
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(rel, entry.Name())
		raw, err := providerfs.ReadRepoFile(root, path)
		if err != nil {
			hookWarn("PR discovery queue read failed: %v", err)
			return false
		}
		var item prDiscovery
		if json.Unmarshal(raw, &item) != nil || len(item.SHAs) == 0 || len(item.SHAs) > 200 {
			hookWarn("PR discovery record is corrupt: %s", entry.Name())
			return false
		}
		if item.Origin == origin && item.Branch == branch {
			work = append(work, pending{path, item})
		}
	}
	sort.Slice(work, func(i, j int) bool { return work[i].item.CreatedAt.Before(work[j].item.CreatedAt) })
	reflected := false
	for i, w := range work {
		if i >= 4 || ctx.Err() != nil {
			break
		}
		did, done := processMergedPRContexts(ctx, resolver, syncer, cwd, branch, origin, w.item.SHAs)
		reflected = reflected || did
		if !done {
			break
		}
		if err := providerfs.RemoveRepoFile(root, w.path); err != nil {
			hookWarn("PR discovery acknowledgement failed: %v", err)
			break
		}
	}
	return reflected
}

// Ordinary successful push/pull also retries saved discovery. This does not read
// ORIG_HEAD or create a new range; it only processes previously durable evidence.
func replaySavedPRDiscovery(ctx context.Context, c *Container, cwd string) bool {
	if c.PRMerges == nil || c.Sync == nil {
		return false
	}
	return replayPRDiscovery(ctx, c.PRMerges, c.Sync, cwd, gitOut(cwd, "rev-parse", "--abbrev-ref", "HEAD"), gitOut(cwd, "config", "--get", "remote.origin.url"), nil)
}
