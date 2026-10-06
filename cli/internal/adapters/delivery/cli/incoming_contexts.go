package cli

import (
	"context"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type incomingContextFetch struct {
	inbound.SyncOutput
	requireBranchPlan bool
}

// fetchIncomingContexts narrows automatic discovery only. Explicit fetch/pull
// and adapters without selected transfer retain their complete recovery path.
func fetchIncomingContexts(ctx context.Context, c *Container, cwd, branch string, shas []string) (incomingContextFetch, error) {
	in := inbound.SyncInput{Cwd: cwd, FetchOnly: true}
	discovery, ok := c.Sync.(inbound.IncomingContextDiscovery)
	var snapshots []domain.Snapshot
	var supported bool
	if ok && branch != "" && branch != "HEAD" {
		var err error
		snapshots, supported, err = discovery.DiscoverIncomingSnapshots(ctx, in)
		if err != nil {
			return incomingContextFetch{}, err
		}
	}
	if !supported {
		out, err := c.Sync.Pull(ctx, in)
		return incomingContextFetch{SyncOutput: out}, err
	}
	candidates := mergedContextCandidates(snapshots, loadRewrites(cwd), shas)
	in.Ref, in.RequireBranchPlan = branch, true
	var result inbound.SyncOutput
	// Every candidate needs hydration now that discovery does not import bodies.
	// Batches stay separate observations; never union their graphs as one proof.
	for start := 0; ; start += domain.MaxBranchPullRoots {
		if err := ctx.Err(); err != nil {
			return incomingContextFetch{}, err
		}
		end := min(start+domain.MaxBranchPullRoots, len(candidates))
		in.ObservationRoots = nil
		for _, snap := range candidates[start:end] {
			in.ObservationRoots = append(in.ObservationRoots, snap.ID)
		}
		out, err := c.Sync.Pull(ctx, in)
		if err != nil {
			return incomingContextFetch{}, err
		}
		count := result.Pulled + out.Pulled
		result = out
		result.Pulled = count
		if end == len(candidates) {
			return incomingContextFetch{SyncOutput: result, requireBranchPlan: true}, nil
		}
	}
}

func mergedContextCandidates(snapshots []domain.Snapshot, rewrites map[string]string, shas []string) []domain.Snapshot {
	// [git <sha>] link → snapshot. Multiple snapshots for the same commit are the latest (same as refSync).
	type linked struct {
		sha  string
		snap domain.Snapshot
	}
	var links []linked
	for _, snap := range snapshots { // newest first
		m := gitLinkRe.FindStringSubmatch(snap.Message)
		if m == nil {
			continue
		}
		links = append(links, linked{sha: resolveRewritten(rewrites, m[1]), snap: snap})
	}
	// Collect candidates in insertion commit order (oldest first) — ensure final tip is the latest merge.
	seen := map[domain.ContentHash]bool{}
	var cands []domain.Snapshot
	for _, sha := range shas {
		for _, l := range links {
			if l.sha == "" || !strings.HasPrefix(sha, l.sha) {
				continue
			}
			if !seen[l.snap.ID] {
				seen[l.snap.ID] = true
				cands = append(cands, l.snap)
			}
			break // only the latest snapshot
		}
	}
	return cands
}
