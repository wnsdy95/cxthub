package cli

import (
	"context"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// fetchGapResolver answers like a provider: only pull requests whose merge
// commit is in the asked range, and it remembers every commit it was asked for.
type fetchGapResolver struct {
	merged map[string]int
	asked  []string
}

func (resolver *fetchGapResolver) ResolveMergedPullRequests(_ context.Context, _, base string, shas []string) ([]outbound.MergedPullRequest, error) {
	resolver.asked = append(resolver.asked, shas...)
	var pulls []outbound.MergedPullRequest
	for _, sha := range shas {
		if number, ok := resolver.merged[sha]; ok {
			pulls = append(pulls, outbound.MergedPullRequest{Number: number, BaseBranch: base, HeadBranch: "feature", MergeCommitSHA: sha})
		}
	}
	return pulls, nil
}

func fetchGapCommit(t *testing.T, source, message string) string {
	t.Helper()
	activationTestGit(t, source, "commit", "--allow-empty", "-m", message)
	return activationTestGit(t, source, "rev-parse", "HEAD")
}

// fetchWithoutHooks moves the remote-tracking ref the way a tool that disables
// hooks does: no reference transaction reaches the observer.
func fetchWithoutHooks(t *testing.T, root string) {
	t.Helper()
	activationTestGit(t, root, "-c", "core.hooksPath="+os.DevNull, "fetch", "origin")
}

func drainFetchObservations(t *testing.T, container *Container, root string) {
	t.Helper()
	for pass := 0; pass < 8; pass++ {
		pending, err := replayFetchObservations(context.Background(), container, root)
		if err != nil {
			t.Fatal(err)
		}
		if !pending {
			return
		}
	}
	t.Fatal("fetch observations never drained")
}

func fetchRanges(t *testing.T, root string) []string {
	t.Helper()
	var ranges []string
	for _, record := range fetchRecords(t, root) {
		ranges = append(ranges, record["before"].(string)[:8]+".."+record["after"].(string)[:8]+" scanned="+map[bool]string{true: "yes", false: "no"}[record["discovery_scanned"] == true]+" "+record["reason"].(string))
	}
	sort.Strings(ranges)
	return ranges
}

// A fetch that runs with hooks disabled moves origin/main unseen. The range it
// skipped must still be searched for merged pull requests once a later hooked
// fetch shows where the observed history resumed.
func TestFetchObservationRecoversRangeOfUnobservedFetch(t *testing.T) {
	root, source, base := fetchFixture(t, true)
	first := fetchGapCommit(t, source, "first merge")
	activationTestGit(t, root, "fetch", "origin")
	hidden := []string{fetchGapCommit(t, source, "merge during an unobserved fetch"), fetchGapCommit(t, source, "second unobserved merge")}
	fetchWithoutHooks(t, root)
	if records := fetchRecords(t, root); len(records) != 1 {
		t.Fatalf("hookless fetch was observed: %v", fetchRanges(t, root))
	}
	last := fetchGapCommit(t, source, "later merge")
	activationTestGit(t, root, "fetch", "origin")
	if records := fetchRecords(t, root); len(records) != 2 {
		t.Fatalf("hooked fetches recorded %d observations", len(records))
	}
	resolver := &fetchGapResolver{merged: map[string]int{first: 1, hidden[0]: 2, hidden[1]: 3, last: 4}}
	queue := &fetchQueueFixture{}
	container := &Container{PRMerges: resolver, Sync: queue}
	drainFetchObservations(t, container, root)
	want := []string{
		base[:8] + ".." + first[:8] + " scanned=yes provider_history_unverified",
		first[:8] + ".." + hidden[1][:8] + " scanned=yes provider_history_unverified",
		hidden[1][:8] + ".." + last[:8] + " scanned=yes provider_history_unverified",
	}
	sort.Strings(want)
	if got := fetchRanges(t, root); !slices.Equal(got, want) {
		t.Fatalf("observed ranges\n got %v\nwant %v", got, want)
	}
	var delivered []int
	for _, pull := range queue.pulls {
		delivered = append(delivered, pull.Number)
	}
	sort.Ints(delivered)
	if !slices.Equal(delivered, []int{1, 2, 3, 4}) {
		t.Fatalf("pull requests handed off: %v", delivered)
	}
	asked := append([]string(nil), resolver.asked...)
	sort.Strings(asked)
	every := []string{first, hidden[0], hidden[1], last}
	sort.Strings(every)
	if !slices.Equal(asked, every) {
		t.Fatalf("each commit must be searched exactly once: %v", asked)
	}
	// The recovered range is durable evidence: later passes add nothing.
	drainFetchObservations(t, container, root)
	if got := fetchRanges(t, root); !slices.Equal(got, want) || len(queue.pulls) != 4 || len(resolver.asked) != 4 {
		t.Fatalf("recovered range was rebuilt: %v pulls=%d asked=%d", got, len(queue.pulls), len(resolver.asked))
	}
	// A later observed fetch continues from the last observation, not the gap.
	final := fetchGapCommit(t, source, "final merge")
	resolver.merged[final] = 5
	activationTestGit(t, root, "fetch", "origin")
	drainFetchObservations(t, container, root)
	if records := fetchRecords(t, root); len(records) != 4 || len(queue.pulls) != 5 || len(resolver.asked) != 5 {
		t.Fatalf("continuous history produced a range: %v pulls=%d asked=%d", fetchRanges(t, root), len(queue.pulls), len(resolver.asked))
	}
	if head := activationTestGit(t, root, "rev-parse", "HEAD"); head != base {
		t.Fatal("recovery moved worktree HEAD")
	}
}

// When the unseen fetch rewrote the branch there is no forward range to
// search. The break is recorded once as already handled, never searched, and
// never judged again.
func TestFetchObservationUnobservedRewriteIsRecordedOnce(t *testing.T) {
	root, source, base := fetchFixture(t, true)
	abandoned := fetchGapCommit(t, source, "abandoned head")
	activationTestGit(t, root, "fetch", "origin")
	activationTestGit(t, source, "reset", "--hard", base)
	rewritten := fetchGapCommit(t, source, "rewritten head")
	fetchWithoutHooks(t, root)
	last := fetchGapCommit(t, source, "later merge")
	activationTestGit(t, root, "fetch", "origin")
	resolver := &fetchGapResolver{merged: map[string]int{abandoned: 1, rewritten: 2, last: 3}}
	queue := &fetchQueueFixture{}
	container := &Container{PRMerges: resolver, Sync: queue}
	drainFetchObservations(t, container, root)
	want := []string{
		abandoned[:8] + ".." + rewritten[:8] + " scanned=yes unobserved_range_rewritten",
		base[:8] + ".." + abandoned[:8] + " scanned=yes provider_history_unverified",
		rewritten[:8] + ".." + last[:8] + " scanned=yes provider_history_unverified",
	}
	sort.Strings(want)
	if got := fetchRanges(t, root); !slices.Equal(got, want) {
		t.Fatalf("observed ranges\n got %v\nwant %v", got, want)
	}
	if slices.Contains(resolver.asked, rewritten) || len(queue.pulls) != 2 {
		t.Fatalf("rewritten range was searched: asked=%v pulls=%d", resolver.asked, len(queue.pulls))
	}
	asked := len(resolver.asked)
	drainFetchObservations(t, container, root)
	if got := fetchRanges(t, root); !slices.Equal(got, want) || len(resolver.asked) != asked {
		t.Fatalf("rewrite was judged again: %v", got)
	}
}

// Without an earlier observation of the ref there is no known start, and a
// range is never invented from the repository's history.
func TestFetchObservationFirstObservationHasNoRange(t *testing.T) {
	root, source, _ := fetchFixture(t, true)
	fetchGapCommit(t, source, "unobserved merge")
	fetchWithoutHooks(t, root)
	last := fetchGapCommit(t, source, "observed merge")
	activationTestGit(t, root, "fetch", "origin")
	resolver := &fetchGapResolver{merged: map[string]int{last: 1}}
	queue := &fetchQueueFixture{}
	drainFetchObservations(t, &Container{PRMerges: resolver, Sync: queue}, root)
	if records := fetchRecords(t, root); len(records) != 1 || len(queue.pulls) != 1 || !slices.Equal(resolver.asked, []string{last}) {
		t.Fatalf("first observation produced a range: %v asked=%v", fetchRanges(t, root), resolver.asked)
	}
}

// Another ref's observations, deletions and re-creations never supply a start.
func TestFetchObservationRangesStayWithinOneRef(t *testing.T) {
	root, source, base := fetchFixture(t, true)
	activationTestGit(t, source, "branch", "side")
	first := fetchGapCommit(t, source, "main moves")
	activationTestGit(t, root, "fetch", "origin")
	activationTestGit(t, source, "branch", "-D", "side")
	activationTestGit(t, root, "fetch", "--prune", "origin")
	activationTestGit(t, source, "branch", "side", first)
	activationTestGit(t, root, "fetch", "origin")
	resolver := &fetchGapResolver{merged: map[string]int{}}
	drainFetchObservations(t, &Container{PRMerges: resolver, Sync: &fetchQueueFixture{}}, root)
	for _, line := range fetchRanges(t, root) {
		if strings.Contains(line, "unobserved_range") {
			t.Fatalf("a range crossed refs or a deletion: %v", fetchRanges(t, root))
		}
	}
	zero := strings.Repeat("0", 8)
	created, removed, recreated := 0, 0, 0
	for _, record := range fetchRecords(t, root) {
		if record["ref"] != "refs/remotes/origin/side" {
			continue
		}
		switch before, after := record["before"].(string)[:8], record["after"].(string)[:8]; {
		case before == zero && after == base[:8]:
			created++
		case after == zero && (before == base[:8] || before == zero):
			// A pruned ref is reported without its previous value.
			removed++
		case before == zero && after == first[:8]:
			recreated++
		default:
			t.Fatalf("unexpected side observation %s..%s", before, after)
		}
	}
	if created != 1 || removed != 1 || recreated != 1 {
		t.Fatalf("side observations created=%d removed=%d recreated=%d", created, removed, recreated)
	}
}
