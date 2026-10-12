package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type fetchPacingPager struct {
	fakePRMergeResolver
	lastPage     int
	failures     int
	includePulls bool
	pages        []int
	checkpoint   func()
}

func (pager *fetchPacingPager) ResolveMergedPullRequestsPage(_ context.Context, _, branch string, shas []string, page int) ([]outbound.MergedPullRequest, int, error) {
	pager.pages = append(pager.pages, page)
	if pager.failures > 0 {
		pager.failures--
		return nil, 0, errors.New("provider unavailable")
	}
	if pager.checkpoint != nil {
		pager.checkpoint()
	}
	var pulls []outbound.MergedPullRequest
	if pager.includePulls || page == pager.lastPage {
		pulls = []outbound.MergedPullRequest{{Number: 4000 + page, BaseBranch: branch, HeadBranch: "feature", MergeCommitSHA: shas[0]}}
	}
	if page < pager.lastPage {
		return pulls, page + 1, nil
	}
	return pulls, 0, nil
}

type fetchPacingQueue struct {
	fetchQueueFixture
	pending bool
}

func (queue *fetchPacingQueue) ReplayPRDeliveries(context.Context, inbound.SyncInput) (bool, error) {
	return queue.pending, nil
}

func TestFetchPacingSuccessfulPagesRemainBounded(t *testing.T) {
	root, source, before := fetchFixture(t, true)
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "incoming")
	after := activationTestGit(t, source, "rev-parse", "HEAD")
	activationTestGit(t, root, "fetch", "origin")
	_, binding, err := readFetchBinding(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	evidence := fetchEvidence{fetchBinding: binding, Before: before, After: after, Ref: "refs/remotes/origin/main"}
	file := prDiscoveryFile(binding.Origin, "main", []string{after}, fetchIdentity(evidence))
	pager := &fetchPacingPager{lastPage: 45}
	queue := &fetchQueueFixture{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	waits, slept := 0, time.Duration(0)
	err = runFetchDiscoveryWorkerWithWait(ctx, &Container{PRMerges: pager, Sync: queue}, root, func(ctx context.Context, delay time.Duration) error {
		waits++
		slept += delay
		if waits >= pager.lastPage || delay != time.Second {
			t.Fatalf("healthy page %d escalated or looped: delay=%s", waits, delay)
		}
		raw, err := providerfs.ReadRepoFile(root, file)
		var checkpoint prDiscovery
		if err != nil || json.Unmarshal(raw, &checkpoint) != nil || checkpoint.ProviderPage != waits+1 {
			t.Fatalf("paced without durable page progress: checkpoint=%+v err=%v", checkpoint, err)
		}
		return ctx.Err()
	})
	if err != nil || waits != 44 || slept != 44*time.Second || len(pager.pages) != 45 || len(queue.pulls) != 1 || queue.pulls[0].Number != 4045 {
		t.Fatalf("last page not reached at bounded pacing: waits=%d sleep=%s calls=%v pulls=%v err=%v", waits, slept, pager.pages, queue.pulls, err)
	}
	for index, page := range pager.pages {
		if page != index+1 {
			t.Fatalf("provider cursor did not advance: %v", pager.pages)
		}
	}
	if _, err := providerfs.ReadRepoFile(root, file); !os.IsNotExist(err) {
		t.Fatalf("completed discovery remains queued: %v", err)
	}
	if record := fetchRecords(t, root)[0]; record["cursor"] != float64(1) || record["discovery_scanned"] != true || record["delivery_state"] != "drained" || record["status"] != "unknown" {
		t.Fatalf("pacing changed completion or evidence: %v", record)
	}
}

func TestFetchPacingStallsBackOffAndProgressResets(t *testing.T) {
	for _, stalled := range []string{"provider", "handoff", "delivery"} {
		t.Run(stalled, func(t *testing.T) {
			root, source, _ := fetchFixture(t, true)
			activationTestGit(t, source, "commit", "--allow-empty", "-m", "incoming")
			activationTestGit(t, root, "fetch", "origin")
			pager := &fetchPacingPager{lastPage: 3}
			queue := &fetchPacingQueue{}
			switch stalled {
			case "provider":
				pager.failures = 8
			case "handoff":
				pager.includePulls = true
				queue.err = errors.New("handoff unavailable")
			case "delivery":
				pager.lastPage = 1
				queue.pending = true
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			delays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
			if stalled != "delivery" {
				delays = append(delays, time.Second, time.Second)
			}
			waits := 0
			err := runFetchDiscoveryWorkerWithWait(ctx, &Container{PRMerges: pager, Sync: queue}, root, func(ctx context.Context, delay time.Duration) error {
				if waits >= len(delays) || delay != delays[waits] {
					t.Fatalf("incorrect stall/progress pacing at pass %d: delay=%s want=%v", waits+1, delay, delays)
				}
				waits++
				if waits <= 8 && stalled != "delivery" && pager.pages[len(pager.pages)-1] != 1 {
					t.Fatalf("failed page or handoff advanced cursor: %v", pager.pages)
				}
				if waits == 8 {
					queue.err, queue.pending = nil, false
				}
				return ctx.Err()
			})
			if err != nil || waits != len(delays) {
				t.Fatalf("stalled work did not recover: waits=%d err=%v", waits, err)
			}
			if stalled == "delivery" && len(pager.pages) != 1 {
				t.Fatalf("pending delivery repeated discovery: %v", pager.pages)
			}
		})
	}
}

func TestFetchPacingWaitHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitFetchDiscovery(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("pacing ignored cancellation: %v", err)
	}
}

func TestFetchPacingFailedCheckpointIsNotProgress(t *testing.T) {
	root, _, _ := fetchFixture(t, true)
	ctx := context.Background()
	origin, branch := "https://github.com/fixture/repo.git", "main"
	shas := []string{strings.Repeat("a", 40)}
	file := prDiscoveryFile(origin, branch, shas, "observation")
	if err := persistPRDiscovery(ctx, root, branch, origin, shas, "observation"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "unchanged")
	if err := os.WriteFile(outside, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	pager := &fetchPacingPager{lastPage: 2, checkpoint: func() {
		if err := os.Remove(filepath.Join(root, file)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, file)); err != nil {
			t.Fatal(err)
		}
	}}
	_, progressed := replayPRDiscoveryWithProgress(ctx, pager, &fetchQueueFixture{}, root, branch, origin, nil, "observation")
	if progressed || len(pager.pages) != 1 || pager.pages[0] != 1 {
		t.Fatalf("unsaved cursor reported progress: progressed=%v pages=%v", progressed, pager.pages)
	}
	if raw, err := os.ReadFile(outside); err != nil || string(raw) != "unchanged" {
		t.Fatalf("failed checkpoint modified symlink target: %q %v", raw, err)
	}
}
