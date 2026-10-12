package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"syscall"
	"time"
)

type prDiscovery struct {
	ProviderPage int       `json:"provider_page,omitempty"`
	Observation  string    `json:"observation,omitempty"`
	Origin       string    `json:"origin"`
	Branch       string    `json:"branch"`
	SHAs         []string  `json:"shas"`
	CreatedAt    time.Time `json:"created_at"`
	LastAttempt  time.Time `json:"last_attempt,omitempty"`
}

func prDiscoveryFile(origin, branch string, shas []string, observation string) string {
	identity := []any{origin, branch, shas}
	if observation != "" {
		identity = append(identity, observation)
	}
	raw, _ := json.Marshal(identity)
	return filepath.Join(".cxt", "pr-discovery", fmt.Sprintf("%x.json", sha256.Sum256(raw)))
}

// persistPRDiscovery records the Git range before any remote IO can consume the
// hook deadline. The next merge may overwrite ORIG_HEAD even when fetch fails.
func persistPRDiscovery(ctx context.Context, cwd, branch, origin string, shas []string, scope ...string) error {
	if branch == "" || branch == "HEAD" || origin == "" || len(shas) == 0 {
		return nil
	}
	root := cxtRepoRoot(ctx, cwd)
	observation := ""
	if len(scope) > 0 {
		observation = scope[0]
	}
	for start := 0; start < len(shas); start += 200 {
		chunk := shas[start:min(start+200, len(shas))]
		file := prDiscoveryFile(origin, branch, chunk, observation)
		_, err := providerfs.WithCxtLock(ctx, root, "pr-discovery", filepath.Base(file), syscall.LOCK_EX, true, func() error {
			if raw, err := providerfs.ReadRepoFile(root, file); err == nil {
				var saved prDiscovery
				if json.Unmarshal(raw, &saved) != nil || saved.ProviderPage < 0 || saved.Observation != observation || saved.Origin != origin || saved.Branch != branch || !slices.Equal(saved.SHAs, chunk) {
					return fmt.Errorf("persist PR discovery: existing range %s is corrupt: %w", file, domain.ErrHashMismatch)
				}
				return nil
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("persist PR discovery: %w", err)
			}
			raw, _ := json.Marshal(prDiscovery{Observation: observation, Origin: origin, Branch: branch, SHAs: chunk, CreatedAt: time.Now().UTC()})
			return providerfs.WriteRepoFileDurable(root, file, raw, 0o600)
		})
		if err != nil {
			return fmt.Errorf("persist PR discovery: %w", err)
		}
	}
	return nil
}

func replayPRDiscovery(ctx context.Context, resolver outbound.PullRequestMergeResolver, syncer mergedPRContextSync, cwd, branch, origin string, shas []string, scope ...string) bool {
	reflected, _ := replayPRDiscoveryWithProgress(ctx, resolver, syncer, cwd, branch, origin, shas, scope...)
	return reflected
}

func replayPRDiscoveryWithProgress(ctx context.Context, resolver outbound.PullRequestMergeResolver, syncer mergedPRContextSync, cwd, branch, origin string, shas []string, scope ...string) (bool, bool) {
	if resolver == nil || syncer == nil || branch == "" || branch == "HEAD" || origin == "" {
		return false, false
	}
	if err := persistPRDiscovery(ctx, cwd, branch, origin, shas, scope...); err != nil {
		hookWarn("%v", err)
	}
	root := cxtRepoRoot(ctx, cwd)
	rel := filepath.Join(".cxt", "pr-discovery")
	// PrepareRepoFile validates every parent before listing the directory.
	probe, err := providerfs.PrepareRepoFile(root, filepath.Join(rel, "probe"), 0o700)
	if err != nil {
		hookWarn("PR discovery queue unavailable: %v", err)
		return false, false
	}
	entries, err := os.ReadDir(filepath.Dir(probe))
	if err != nil {
		hookWarn("PR discovery queue unavailable: %v", err)
		return false, false
	}
	type pending struct {
		path string
		item prDiscovery
	}
	var work []pending
	observation := ""
	if len(scope) > 0 {
		observation = scope[0]
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(rel, entry.Name())
		raw, err := providerfs.ReadRepoFile(root, path)
		if err != nil {
			hookWarn("PR discovery queue read failed: %v", err)
			continue
		}
		var item prDiscovery
		if json.Unmarshal(raw, &item) != nil || item.ProviderPage < 0 || len(item.SHAs) == 0 || len(item.SHAs) > 200 {
			hookWarn("PR discovery record is corrupt: %s", entry.Name())
			continue
		}
		if path != prDiscoveryFile(item.Origin, item.Branch, item.SHAs, item.Observation) {
			hookWarn("PR discovery record identity is corrupt: %s", entry.Name())
			continue
		}
		if item.Origin == origin && item.Branch == branch && item.Observation == observation {
			work = append(work, pending{path, item})
		}
	}
	sort.Slice(work, func(i, j int) bool {
		a, b := work[i].item, work[j].item
		if !a.LastAttempt.Equal(b.LastAttempt) {
			return a.LastAttempt.Before(b.LastAttempt)
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return work[i].path < work[j].path
	})
	reflected, progressed := false, false
	for i, w := range work {
		if i >= 4 || ctx.Err() != nil {
			break
		}
		_, err := providerfs.WithCxtLock(ctx, root, "pr-discovery", filepath.Base(w.path), syscall.LOCK_EX, true, func() error {
			raw, err := providerfs.ReadRepoFile(root, w.path)
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			var item prDiscovery
			if json.Unmarshal(raw, &item) != nil || item.ProviderPage < 0 || item.Origin != origin || item.Branch != branch || item.Observation != observation || w.path != prDiscoveryFile(item.Origin, item.Branch, item.SHAs, item.Observation) || len(item.SHAs) == 0 || len(item.SHAs) > 200 {
				return domain.ErrHashMismatch
			}
			item.LastAttempt = time.Now().UTC()
			raw, _ = json.Marshal(item)
			if err := providerfs.WriteRepoFileDurable(root, w.path, raw, 0600); err != nil {
				return err
			}
			did, done := false, false
			pager, paged := resolver.(outbound.PullRequestMergePager)
			_, queued := syncer.(fetchPRQueue)
			if paged && queued {
				page := max(1, item.ProviderPage)
				pulls, next, err := pager.ResolveMergedPullRequestsPage(ctx, origin, branch, item.SHAs, page)
				if err != nil {
					return err
				}
				if next != 0 && next <= page {
					return domain.ErrSyncConflict
				}
				did, done = processResolvedPRContexts(ctx, syncer, cwd, branch, pulls)
				if done && next != 0 {
					item.ProviderPage = next
					raw, _ := json.Marshal(item)
					if err := providerfs.WriteRepoFileDurable(root, w.path, raw, 0600); err != nil {
						return err
					}
					progressed = true
					return nil
				}
			} else {
				did, done = processMergedPRContexts(ctx, resolver, syncer, cwd, branch, origin, item.SHAs)
			}
			reflected = reflected || did
			if !done {
				return nil
			}
			if err := providerfs.RemoveRepoFile(root, w.path); err != nil {
				return err
			}
			progressed = true
			return nil
		})
		if err != nil {
			hookWarn("PR discovery remains pending: %v", err)
		}
	}
	return reflected, progressed
}

// Ordinary successful push/pull also retries saved discovery. This does not read
// ORIG_HEAD or create a new range; it only processes previously durable evidence.
func replaySavedPRDiscovery(ctx context.Context, c *Container, cwd string) bool {
	if c.WakeFetchDiscovery != nil {
		c.WakeFetchDiscovery(cwd)
	}
	if c.PRMerges == nil || c.Sync == nil {
		return false
	}
	return replayPRDiscovery(ctx, c.PRMerges, c.Sync, cwd, gitOut(cwd, "rev-parse", "--abbrev-ref", "HEAD"), gitOut(cwd, "config", "--get", "remote.origin.url"), nil)
}
