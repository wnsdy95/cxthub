package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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
	if err := runSegmentCollector(context.Background(), segmentCollectorTestContainer(t, &requests, &runs, &state), cwd); err != nil || runs != 0 {
		t.Fatalf("a folder outside Git: runs=%d err=%v", runs, err)
	}
	activationTestGit(t, cwd, "init", "-b", "main")
	if err := runSegmentCollector(context.Background(), segmentCollectorTestContainer(t, &requests, &runs, &state), cwd); err != nil || runs != 0 {
		t.Fatalf("a repository that was not set up: runs=%d err=%v", runs, err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, ".cxt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".cxt", "HEAD"), []byte("cxt\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := segmentCollectorTestContainer(t, &requests, &runs, &state)
	if err := runSegmentCollector(context.Background(), c, cwd); err != nil || runs != 1 {
		t.Fatalf("no request: runs=%d err=%v", runs, err)
	}
	// Two requests arrived while it ran: two more runs, then it ends.
	requests, runs = 2, 0
	if err := runSegmentCollector(context.Background(), c, cwd); err != nil || runs != 3 || requests != 0 {
		t.Fatalf("two requests: runs=%d left=%d err=%v", runs, requests, err)
	}
	// The collector is the same whatever woke it: a word after its name,
	// which a hook script of an earlier version passed, changes nothing.
	for _, arguments := range []string{"segment-collect", "segment-collect after-push", "segment-collect another-word"} {
		requests, runs, state.LastRun = 0, 0, nil
		if err := runGitHook(context.Background(), c, cwd, strings.Fields(arguments)); err != nil || runs != 1 {
			t.Fatalf("cxt git-hook %s: runs=%d: %v", arguments, runs, err)
		}
	}
	// A run that just happened is waited out before the next one starts.
	runs = 0
	state.LastRun = &domain.CollectorRun{At: time.Now().UTC().Format(time.RFC3339Nano)}
	started := time.Now()
	if err := runSegmentCollector(context.Background(), c, cwd); err != nil || runs != 1 || time.Since(started) < 15*time.Millisecond {
		t.Fatalf("spacing: runs=%d waited=%s err=%v", runs, time.Since(started), err)
	}
	// A push of this clone has ended: the pause is not waited out, however
	// long it is, and the note of the push is taken by the run that answers it.
	poll := segmentCollectorPushPoll
	segmentCollectorPushPoll = 5 * time.Millisecond
	defer func() { segmentCollectorPushPoll = poll }()
	segmentCollectorInterval = time.Hour
	var pushes atomic.Int32
	c.TakeSegmentCollectorPush = func(_ context.Context, where string) bool {
		if where != cwd {
			t.Errorf("asked for the pushes of %q", where)
		}
		return pushes.Load() > 0 && pushes.Add(-1) >= 0
	}
	runs = 0
	state.LastRun = &domain.CollectorRun{At: time.Now().UTC().Format(time.RFC3339Nano)}
	pushes.Store(1)
	started = time.Now()
	if err := runSegmentCollector(context.Background(), c, cwd); err != nil || runs != 1 || time.Since(started) > 5*time.Second || pushes.Load() != 0 {
		t.Fatalf("a push before the pause: runs=%d waited=%s notes left=%d err=%v", runs, time.Since(started), pushes.Load(), err)
	}
	// A push that ends while the collector waits ends the wait.
	runs = 0
	state.LastRun = &domain.CollectorRun{At: time.Now().UTC().Format(time.RFC3339Nano)}
	go func() {
		time.Sleep(60 * time.Millisecond)
		pushes.Store(1)
	}()
	started = time.Now()
	if err := runSegmentCollector(context.Background(), c, cwd); err != nil || runs != 1 || time.Since(started) < 50*time.Millisecond || time.Since(started) > 5*time.Second {
		t.Fatalf("a push during the pause: runs=%d waited=%s err=%v", runs, time.Since(started), err)
	}
	// Without a push the pause is waited out: nothing runs in the meantime.
	runs = 0
	state.LastRun = &domain.CollectorRun{At: time.Now().UTC().Format(time.RFC3339Nano)}
	waiting, stop := context.WithTimeout(context.Background(), 80*time.Millisecond)
	if err := runSegmentCollector(waiting, c, cwd); err != nil || runs != 0 {
		t.Fatalf("no push during the pause: runs=%d err=%v", runs, err)
	}
	stop()
	// A push noted when no pause is due is taken by the run all the same,
	// so that it does not end a later pause.
	runs, state.LastRun = 0, nil
	pushes.Store(1)
	if err := runSegmentCollector(context.Background(), c, cwd); err != nil || runs != 1 || pushes.Load() != 0 {
		t.Fatalf("a push when no pause is due: runs=%d notes left=%d err=%v", runs, pushes.Load(), err)
	}
	c.TakeSegmentCollectorPush = nil
	// Cancelled while waiting: no run.
	runs = 0
	segmentCollectorInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runSegmentCollector(ctx, c, cwd); err != nil || runs != 0 {
		t.Fatalf("cancelled: runs=%d err=%v", runs, err)
	}
}

// The pause itself, without the programs around it: it is waited out to its
// end, a push that has ended ends it, and the note of a push is asked for
// also when no pause is due.
func TestSegmentCollectorPauseIsWaitedOutUnlessAPushEnded(t *testing.T) {
	poll := segmentCollectorPushPoll
	segmentCollectorPushPoll = 10 * time.Millisecond
	defer func() { segmentCollectorPushPoll = poll }()
	asked := 0
	never := func() bool { asked++; return false }
	started := time.Now()
	if err := segmentCollectorPause(context.Background(), 120*time.Millisecond, never); err != nil || time.Since(started) < 110*time.Millisecond || asked < 6 {
		t.Fatalf("a pause without a push: waited %s, looked for a push %d times, err=%v", time.Since(started), asked, err)
	}
	// A push at the third look: the pause ends there, long before its end.
	asked = 0
	third := func() bool { asked++; return asked == 3 }
	started = time.Now()
	if err := segmentCollectorPause(context.Background(), time.Hour, third); err != nil || asked != 3 || time.Since(started) > 5*time.Second {
		t.Fatalf("a push at the third look: waited %s, looked %d times, err=%v", time.Since(started), asked, err)
	}
	// No pause is due: the note is asked for once, and nothing is waited.
	asked = 0
	if err := segmentCollectorPause(context.Background(), 0, never); err != nil || asked != 1 {
		t.Fatalf("no pause due: looked %d times, err=%v", asked, err)
	}
	// Stopped while it waits: it says so, and the collector does not run.
	stopped, stop := context.WithCancel(context.Background())
	stop()
	if err := segmentCollectorPause(stopped, time.Hour, never); err == nil {
		t.Fatal("a stopped pause ended as if it had been waited out")
	}
}

func TestSegmentCollectorDoesNothingWithoutAServerOrItsParts(t *testing.T) {
	cwd := t.TempDir()
	requests, runs, state := 0, 0, domain.CollectorState{}
	c := segmentCollectorTestContainer(t, &requests, &runs, &state)
	// No remote is configured for the repository: nothing to upload to.
	t.Setenv("CXT_REMOTE", "")
	if err := runSegmentCollector(context.Background(), c, cwd); err != nil || runs != 0 {
		t.Fatalf("without a remote: runs=%d err=%v", runs, err)
	}
	t.Setenv("CXT_REMOTE", "https://synthetic.invalid/api/v1")
	for name, broken := range map[string]*Container{
		"no container": nil,
		"no collector": {WithSegmentCollectorLock: c.WithSegmentCollectorLock},
		"no lock":      {SegmentCollector: c.SegmentCollector},
	} {
		if err := runSegmentCollector(context.Background(), broken, cwd); err != nil || runs != 0 {
			t.Fatalf("%s: runs=%d err=%v", name, runs, err)
		}
	}
	// Another collector holds the lock: this one leaves and does not run.
	busy := &Container{SegmentCollector: c.SegmentCollector, WithSegmentCollectorLock: func(context.Context, string, func(func() bool) error) (bool, error) {
		return false, nil
	}}
	if err := runSegmentCollector(context.Background(), busy, cwd); err != nil || runs != 0 {
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
	var pushing []int
	c := &Container{WakeSegmentCollector: func(cwd string) { woken = append(woken, cwd) },
		WakeSegmentCollectorAfterPush: func(cwd string, process int) {
			wokenAfterPush, pushing = append(wokenAfterPush, cwd), append(pushing, process)
		}}

	// cxt is not set up here: the hook leaves the repository alone.
	for _, event := range []string{"post-merge", "pre-push"} {
		if err := runGitHook(context.Background(), c, repo, []string{"wake", event, "4321"}); err != nil || len(woken) != 0 || len(wokenAfterPush) != 0 {
			t.Fatalf("an unconnected repository: %s: %v %v %v", event, woken, wokenAfterPush, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Every hook script says which hook it is and which process runs it.
	for _, event := range []string{"post-commit", "post-checkout", "post-merge", "post-rewrite"} {
		woken = nil
		if err := runGitHook(context.Background(), c, repo, []string{"wake", event, "4321"}); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		if len(woken) != 1 || woken[0] != repo || len(wokenAfterPush) != 0 {
			t.Fatalf("%s woke %v, and for a push %v", event, woken, wokenAfterPush)
		}
	}
	// Git runs pre-push before the push and has no hook after it: that hook
	// wakes no collector now. Its wake is held until the process that runs
	// the hook, the one that pushes, has ended.
	woken = nil
	if err := runGitHook(context.Background(), c, repo, []string{"wake", "pre-push", "4321"}); err != nil ||
		len(woken) != 0 || len(wokenAfterPush) != 1 || wokenAfterPush[0] != repo || pushing[0] != 4321 {
		t.Fatalf("pre-push woke %v, and for the push of %v %v: %v", woken, pushing, wokenAfterPush, err)
	}
	// A pre-push that names no process to wait for wakes the collector like
	// any hook: the script of an earlier version, which named none; what is
	// no number of a process; and words that are not the script's.
	for _, words := range [][]string{{"wake", "pre-push"}, {"wake", "pre-push", "0"}, {"wake", "pre-push", "1"}, {"wake", "pre-push", "-4321"},
		{"wake", "pre-push", "git"}, {"wake", "pre-push", ""}, {"wake", "pre-push", "99999999999999999999"}, {"wake", "pre-push", "4321", "more"}, {"pre-push", "4321"}, {"pre-push", "pre-push", "4321"}} {
		woken = nil
		if err := runGitHook(context.Background(), c, repo, words); err != nil || len(woken) != 1 || len(wokenAfterPush) != 1 {
			t.Fatalf("%q woke %v, and for a push %v: %v", words, woken, wokenAfterPush, err)
		}
	}
	// Where only the plain wake is wired, a push still wakes the collector.
	plain := &Container{WakeSegmentCollector: func(cwd string) { woken = append(woken, cwd) }}
	woken = nil
	if err := runGitHook(context.Background(), plain, repo, []string{"wake", "pre-push", "4321"}); err != nil || len(woken) != 1 {
		t.Fatalf("pre-push without the wait: %v %v", woken, err)
	}
	// A name this version does not know is one a hook script of an earlier
	// version still calls. It succeeds, so that the script cannot stop Git,
	// and it wakes the collector like any hook.
	for _, earlier := range [][]string{{"branch-transaction", "prepared", "1234"}, {"ref-prepare"}, {"pending-sync"}, {"wake"}, {"after-push"}, {}} {
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

// The wake of a push is held until the process that pushes has ended, and
// then it is the wake of any hook.
func TestWakeAfterPushWaitsForThePushingProcessToEnd(t *testing.T) {
	poll, limit := wakeAfterPushPoll, wakeAfterPushLimit
	t.Cleanup(func() { wakeAfterPushPoll, wakeAfterPushLimit = poll, limit })
	wakeAfterPushPoll = 5 * time.Millisecond
	repo := t.TempDir()
	var woken, noted atomic.Int32
	c := &Container{WakeSegmentCollector: func(cwd string) {
		if cwd != repo {
			t.Errorf("woke the collector of %q", cwd)
		}
		// The push is noted before the collector is woken: the run this
		// wake leads to must find the note.
		if noted.Load() != woken.Load()+1 {
			t.Errorf("the collector was woken for the %d. time after %d pushes were noted", woken.Load()+1, noted.Load())
		}
		woken.Add(1)
	}, NoteSegmentCollectorPush: func(_ context.Context, cwd string) {
		if cwd != repo || woken.Load() != noted.Load() {
			t.Errorf("noted the %d. push of %q after %d wakes", noted.Load()+1, cwd, woken.Load())
		}
		noted.Add(1)
	}}

	// A process that goes on until it is told to end stands in for a push.
	push := exec.Command("sh", "-c", "read line; exit 0")
	end, err := push.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := push.Start(); err != nil {
		t.Fatal(err)
	}
	ended := false
	t.Cleanup(func() {
		if !ended {
			_ = push.Process.Kill()
			_ = push.Wait()
		}
	})
	process := push.Process.Pid
	if !processRuns(process) || !processRuns(os.Getpid()) || !processRuns(1) {
		t.Fatalf("processes that run are said not to: the push %v, this test %v, the first process %v",
			processRuns(process), processRuns(os.Getpid()), processRuns(1))
	}
	done := make(chan error, 1)
	go func() {
		done <- runGitHook(context.Background(), c, repo, []string{"after-push", strconv.Itoa(process)})
	}()
	// Twenty looks at the process later it still runs, and nothing was woken.
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the wait ended while the push went on: %v", err)
	default:
	}
	if got := woken.Load() + noted.Load(); got != 0 {
		t.Fatalf("the collector was woken or the push noted %d times while the push went on", got)
	}
	if err := end.Close(); err != nil {
		t.Fatal(err)
	}
	if err := push.Wait(); err != nil {
		t.Fatal(err)
	}
	ended = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait went on after the push had ended")
	}
	if woken.Load() != 1 || noted.Load() != 1 {
		t.Fatalf("after the push the collector was woken %d times and the push noted %d times", woken.Load(), noted.Load())
	}
	if processRuns(process) {
		t.Fatal("a process that has ended is said to run")
	}

	// A push that had ended before anything looked wakes at once.
	started := time.Now()
	if err := runWakeAfterPush(context.Background(), c, repo, strconv.Itoa(process)); err != nil || woken.Load() != 2 || time.Since(started) > 5*time.Second {
		t.Fatalf("after a push that had ended: woken %d times in %s: %v", woken.Load(), time.Since(started), err)
	}
	// A push that goes on past the limit is not waited for any longer: the
	// collector is woken, and the next wake reads what that push leaves.
	wakeAfterPushLimit = 40 * time.Millisecond
	started = time.Now()
	if err := runWakeAfterPush(context.Background(), c, repo, strconv.Itoa(os.Getpid())); err != nil || woken.Load() != 3 || time.Since(started) < 40*time.Millisecond {
		t.Fatalf("a push past the limit: woken %d times in %s: %v", woken.Load(), time.Since(started), err)
	}
	// What names no process is not waited for, and wakes nothing: the hook
	// that found no process woke the collector itself.
	for _, word := range []string{"", "git", "0", "1", "-1", "-" + strconv.Itoa(os.Getpid()), "99999999999999999999"} {
		if err := runWakeAfterPush(context.Background(), c, repo, word); err != nil || woken.Load() != 3 {
			t.Fatalf("%q: woken %d times: %v", word, woken.Load(), err)
		}
	}
	// Without the wiring of a process the wait ends at once.
	for _, without := range []*Container{nil, {}} {
		if err := runWakeAfterPush(context.Background(), without, repo, strconv.Itoa(os.Getpid())); err != nil {
			t.Fatal(err)
		}
	}
}
