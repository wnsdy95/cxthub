package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type discoveryPager struct {
	fakePRMergeResolver
	pages []int
	fail  bool
}

func (pager *discoveryPager) ResolveMergedPullRequestsPage(_ context.Context, _, branch string, shas []string, page int) ([]outbound.MergedPullRequest, int, error) {
	pager.pages = append(pager.pages, page)
	if pager.fail {
		return nil, 0, errors.New("provider temporarily offline")
	}
	if page < 12 {
		return nil, page + 1, nil
	}
	return []outbound.MergedPullRequest{{Number: 1201, BaseBranch: branch, HeadBranch: "feature", MergeCommitSHA: shas[0]}}, 0, nil
}

func TestPRDiscoveryRetainsProviderCursorAcrossRestartAndFailures(t *testing.T) {
	root, _, _ := fetchFixture(t, true)
	ctx := context.Background()
	origin := "https://github.com/fixture/repo.git"
	shas := []string{strings.Repeat("a", 40)}
	if err := persistPRDiscovery(ctx, root, "main", origin, shas, "observation"); err != nil {
		t.Fatal(err)
	}
	file := prDiscoveryFile(origin, "main", shas, "observation")
	read := func() prDiscovery {
		t.Helper()
		raw, err := providerfs.ReadRepoFile(root, file)
		var item prDiscovery
		if err != nil || json.Unmarshal(raw, &item) != nil {
			t.Fatalf("lost pending discovery: %v", err)
		}
		return item
	}
	queue := &fetchQueueFixture{}
	for page := 1; page <= 11; page++ {
		pager := &discoveryPager{}
		replayPRDiscovery(ctx, pager, queue, root, "main", origin, nil, "observation")
		if item := read(); item.ProviderPage != page+1 || len(pager.pages) != 1 || pager.pages[0] != page {
			t.Fatalf("restart lost page %d: item=%+v called=%v", page, item, pager.pages)
		}
	}
	pager := &discoveryPager{fail: true}
	replayPRDiscovery(ctx, pager, queue, root, "main", origin, nil, "observation")
	if read().ProviderPage != 12 {
		t.Fatal("provider failure advanced cursor")
	}
	pager.fail = false
	queue.err = errors.New("local queue unavailable")
	replayPRDiscovery(ctx, pager, queue, root, "main", origin, nil, "observation")
	if read().ProviderPage != 12 {
		t.Fatal("handoff failure advanced cursor")
	}
	queue.err = nil
	replayPRDiscovery(ctx, pager, queue, root, "main", origin, nil, "observation")
	if _, err := providerfs.ReadRepoFile(root, file); !os.IsNotExist(err) || len(queue.pulls) != 1 || queue.pulls[0].Number != 1201 {
		t.Fatalf("final page not handed off: pulls=%v err=%v", queue.pulls, err)
	}
}

func TestPRDiscoveryConcurrentWorkersPreserveCursor(t *testing.T) {
	root, _, _ := fetchFixture(t, true)
	ctx := context.Background()
	origin := "https://github.com/fixture/repo.git"
	shas := []string{strings.Repeat("a", 40)}
	pager := &discoveryPager{}
	queue := &fetchQueueFixture{}
	var workers sync.WaitGroup
	for count := 0; count < 6; count++ {
		workers.Go(func() {
			if err := persistPRDiscovery(ctx, root, "main", origin, shas, "observation"); err != nil {
				t.Error(err)
				return
			}
			replayPRDiscovery(ctx, pager, queue, root, "main", origin, nil, "observation")
		})
	}
	workers.Wait()
	file := prDiscoveryFile(origin, "main", shas, "observation")
	raw, err := providerfs.ReadRepoFile(root, file)
	var item prDiscovery
	if err != nil || json.Unmarshal(raw, &item) != nil || item.ProviderPage != 7 || len(pager.pages) != 6 {
		t.Fatalf("concurrent progress lost: item=%+v calls=%v err=%v", item, pager.pages, err)
	}
	for index, page := range pager.pages {
		if page != index+1 {
			t.Fatalf("cursor replayed stale page: %v", pager.pages)
		}
	}
}
