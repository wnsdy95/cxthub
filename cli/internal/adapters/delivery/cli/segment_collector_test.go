package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type segmentCollectorFunc func(context.Context, string) (domain.SegmentPushResult, error)

func (f segmentCollectorFunc) CollectSegments(ctx context.Context, cwd string) (domain.SegmentPushResult, error) {
	return f(ctx, cwd)
}

func TestSegmentCollectorWaitSpacesRunsAndBacksOffAfterAFailure(t *testing.T) {
	now := time.Date(2031, 1, 2, 3, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339Nano) }
	for name, test := range map[string]struct {
		last *domain.CollectorRun
		want time.Duration
	}{
		"never ran":                  {nil, 0},
		"ran long ago":               {&domain.CollectorRun{At: at(time.Hour)}, 0},
		"ran four seconds ago":       {&domain.CollectorRun{At: at(4 * time.Second)}, 6 * time.Second},
		"failed ten seconds ago":     {&domain.CollectorRun{At: at(10 * time.Second), Error: "synthetic"}, 10*time.Minute - 10*time.Second},
		"failed long ago":            {&domain.CollectorRun{At: at(time.Hour), Error: "synthetic"}, 0},
		"a time that cannot be read": {&domain.CollectorRun{At: "yesterday"}, 0},
		"a time in the future":       {&domain.CollectorRun{At: now.Add(time.Hour).Format(time.RFC3339Nano)}, 0},
		"exactly one interval ago":   {&domain.CollectorRun{At: at(10 * time.Second)}, 0},
		// A run that took long is followed by six times what it took.
		"a quick run adds nothing":            {&domain.CollectorRun{At: at(4 * time.Second), Millis: 1000}, 6 * time.Second},
		"a ten second run, ten seconds ago":   {&domain.CollectorRun{At: at(10 * time.Second), Millis: 10_000}, 50 * time.Second},
		"a ten second run, a minute ago":      {&domain.CollectorRun{At: at(time.Minute), Millis: 10_000}, 0},
		"a five minute run waits the longest": {&domain.CollectorRun{At: at(time.Minute), Millis: 300_000}, 9 * time.Minute},
		"a failed long run waits the same":    {&domain.CollectorRun{At: at(time.Minute), Millis: 300_000, Error: "synthetic"}, 9 * time.Minute},
	} {
		if got := segmentCollectorWait(domain.CollectorState{LastRun: test.last}, now); got != test.want {
			t.Errorf("%s: %s", name, got)
		}
	}
}

func segmentCollectorTestContainer(t *testing.T, requests *int, runs *int, state *domain.CollectorState) *Container {
	t.Helper()
	return &Container{
		SegmentCollector: segmentCollectorFunc(func(context.Context, string) (domain.SegmentPushResult, error) {
			*runs++
			// A run that fails is still a run: the collector never fails its caller.
			return domain.SegmentPushResult{}, errors.New("synthetic failure")
		}),
		WithSegmentCollectorLock: func(_ context.Context, _ string, use func(another func() bool) error) (bool, error) {
			return true, use(func() bool {
				if *requests > 0 {
					*requests--
					return true
				}
				return false
			})
		},
		SegmentCollectorState: func(context.Context, string) (domain.CollectorState, error) { return *state, nil },
	}
}

func TestSegmentCollectorRunsOnceAndOnceMoreForEachRequest(t *testing.T) {
	t.Setenv("CXT_REMOTE", "https://synthetic.invalid/api/v1")
	previous := segmentCollectorInterval
	segmentCollectorInterval = 20 * time.Millisecond
	defer func() { segmentCollectorInterval = previous }()
	// The collector runs only where cxt was set up.
	cwd := t.TempDir()
	requests, runs, state := 0, 0, domain.CollectorState{}
	if err := runSegmentCollector(context.Background(), segmentCollectorTestContainer(t, &requests, &runs, &state), cwd, false); err != nil || runs != 0 {
		t.Fatalf("a folder outside Git: runs=%d err=%v", runs, err)
	}
	activationTestGit(t, cwd, "init", "-b", "main")
	if err := runSegmentCollector(context.Background(), segmentCollectorTestContainer(t, &requests, &runs, &state), cwd, false); err != nil || runs != 0 {
		t.Fatalf("a repository that was not set up: runs=%d err=%v", runs, err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, ".cxt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".cxt", "HEAD"), []byte("cxt\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := segmentCollectorTestContainer(t, &requests, &runs, &state)
	if err := runSegmentCollector(context.Background(), c, cwd, false); err != nil || runs != 1 {
		t.Fatalf("no request: runs=%d err=%v", runs, err)
	}
	// Two requests arrived while it ran: two more runs, then it ends.
	requests, runs = 2, 0
	if err := runSegmentCollector(context.Background(), c, cwd, false); err != nil || runs != 3 || requests != 0 {
		t.Fatalf("two requests: runs=%d left=%d err=%v", runs, requests, err)
	}
	// Woken by a push: once more after the pause, with no request for it.
	requests, runs, state.LastRun = 0, 0, nil
	if err := runSegmentCollector(context.Background(), c, cwd, true); err != nil || runs != 2 {
		t.Fatalf("after a push: runs=%d err=%v", runs, err)
	}
	// The collector process is told so by the hook that started it.
	for arguments, want := range map[string]int{"segment-collect": 1, "segment-collect after-push": 2, "segment-collect another-word": 1} {
		runs, state.LastRun = 0, nil
		if err := runGitHook(context.Background(), c, cwd, strings.Fields(arguments)); err != nil || runs != want {
			t.Fatalf("cxt git-hook %s: runs=%d, want %d: %v", arguments, runs, want, err)
		}
	}
	// A request that arrived meanwhile is still answered, by a third run.
	requests, runs = 1, 0
	if err := runSegmentCollector(context.Background(), c, cwd, true); err != nil || runs != 3 || requests != 0 {
		t.Fatalf("after a push, with a request: runs=%d left=%d err=%v", runs, requests, err)
	}
	// A run that just happened is waited out before the next one starts.
	runs = 0
	state.LastRun = &domain.CollectorRun{At: time.Now().UTC().Format(time.RFC3339Nano)}
	started := time.Now()
	if err := runSegmentCollector(context.Background(), c, cwd, false); err != nil || runs != 1 || time.Since(started) < 15*time.Millisecond {
		t.Fatalf("spacing: runs=%d waited=%s err=%v", runs, time.Since(started), err)
	}
	// Cancelled while waiting: no run.
	runs = 0
	segmentCollectorInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runSegmentCollector(ctx, c, cwd, false); err != nil || runs != 0 {
		t.Fatalf("cancelled: runs=%d err=%v", runs, err)
	}
}

func TestSegmentCollectorDoesNothingWithoutAServerOrItsParts(t *testing.T) {
	cwd := t.TempDir()
	requests, runs, state := 0, 0, domain.CollectorState{}
	c := segmentCollectorTestContainer(t, &requests, &runs, &state)
	// No remote is configured for the repository: nothing to upload to.
	t.Setenv("CXT_REMOTE", "")
	if err := runSegmentCollector(context.Background(), c, cwd, false); err != nil || runs != 0 {
		t.Fatalf("without a remote: runs=%d err=%v", runs, err)
	}
	t.Setenv("CXT_REMOTE", "https://synthetic.invalid/api/v1")
	for name, broken := range map[string]*Container{
		"no container": nil,
		"no collector": {WithSegmentCollectorLock: c.WithSegmentCollectorLock},
		"no lock":      {SegmentCollector: c.SegmentCollector},
	} {
		if err := runSegmentCollector(context.Background(), broken, cwd, false); err != nil || runs != 0 {
			t.Fatalf("%s: runs=%d err=%v", name, runs, err)
		}
	}
	// Another collector holds the lock: this one leaves and does not run.
	busy := &Container{SegmentCollector: c.SegmentCollector, WithSegmentCollectorLock: func(context.Context, string, func(func() bool) error) (bool, error) {
		return false, nil
	}}
	if err := runSegmentCollector(context.Background(), busy, cwd, false); err != nil || runs != 0 {
		t.Fatalf("while another runs: runs=%d err=%v", runs, err)
	}
}

// A Git hook only wakes the collector, after its own work, in a repository
// cxt is set up in.
func TestGitHooksWakeTheSegmentCollector(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CXT_REMOTE", "")
	repo := t.TempDir()
	activationTestGit(t, repo, "init", "-b", "main")
	activationTestGit(t, repo, "-c", "user.name=cxt test", "-c", "user.email=cxt@example.test", "commit", "--allow-empty", "-m", "first")
	var woken, wokenAfterPush []string
	c := &Container{WakeSegmentCollector: func(cwd string) { woken = append(woken, cwd) },
		WakeSegmentCollectorAfterPush: func(cwd string) { wokenAfterPush = append(wokenAfterPush, cwd) }}

	// cxt is not set up here: the hook leaves the repository alone.
	for _, event := range []string{"post-merge", "pre-push"} {
		if err := runGitHook(context.Background(), c, repo, []string{"wake", event}); err != nil || len(woken) != 0 || len(wokenAfterPush) != 0 {
			t.Fatalf("an unconnected repository: %s: %v %v %v", event, woken, wokenAfterPush, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"post-commit", "post-checkout", "post-merge", "post-rewrite"} {
		woken = nil
		if err := runGitHook(context.Background(), c, repo, []string{"wake", event}); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		if len(woken) != 1 || woken[0] != repo || len(wokenAfterPush) != 0 {
			t.Fatalf("%s woke %v, and for a push %v", event, woken, wokenAfterPush)
		}
	}
	// Git runs pre-push before the push and has no hook after it: that hook
	// wakes a collector that runs once more after its pause, and no other.
	woken = nil
	if err := runGitHook(context.Background(), c, repo, []string{"wake", "pre-push"}); err != nil ||
		len(woken) != 0 || len(wokenAfterPush) != 1 || wokenAfterPush[0] != repo {
		t.Fatalf("pre-push woke %v, and for a push %v: %v", woken, wokenAfterPush, err)
	}
	// Where only the plain wake is wired, a push still wakes the collector.
	plain := &Container{WakeSegmentCollector: func(cwd string) { woken = append(woken, cwd) }}
	if err := runGitHook(context.Background(), plain, repo, []string{"wake", "pre-push"}); err != nil || len(woken) != 1 {
		t.Fatalf("pre-push without the second wake: %v %v", woken, err)
	}
	// A name this version does not know is one a hook script of an earlier
	// version still calls. It succeeds, so that the script cannot stop Git,
	// and it wakes the collector like any hook.
	for _, earlier := range [][]string{{"branch-transaction", "prepared", "1234"}, {"ref-prepare"}, {"pending-sync"}, {"wake"}, {}} {
		woken = nil
		if err := runGitHook(context.Background(), c, repo, earlier); err != nil || len(woken) != 1 || len(wokenAfterPush) != 1 {
			t.Fatalf("%v: %v %v", earlier, woken, err)
		}
	}
	// Without the wiring of a process, a hook still succeeds.
	if err := runGitHook(context.Background(), nil, repo, []string{"post-commit"}); err != nil {
		t.Fatal(err)
	}
}
