package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub-cli/internal/app"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type fetchLivenessRemote struct {
	outbound.RemoteSync
	root  string
	block bool
}

func (remote fetchLivenessRemote) SubmitPRPromotion(ctx context.Context, repo string, request domain.PullRequestMerge) error {
	raw, _ := json.Marshal(struct {
		Repo string
		PR   domain.PullRequestMerge
	}{repo, request})
	if err := providerfs.WriteRepoFileDurable(remote.root, ".cxt/fetch-test-attempt", raw, 0600); err != nil {
		return err
	}
	if remote.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func TestFetchLivenessWorkerProcess(t *testing.T) {
	mode := os.Getenv("CXT_FETCH_LIVENESS_WORKER")
	if mode == "" {
		return
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if mode == "expire" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
	}
	remote := fetchLivenessRemote{root: root, block: mode != "drain"}
	service := app.NewSyncRepoService(storage.NewFileStore(root), remote, nil, storage.NewSyncOutbox())
	err = runGitHook(ctx, &Container{Sync: service}, root, []string{"remote-ref-discovery"})
	if mode == "expire" && errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
}

func fetchLivenessCommand(t *testing.T, root, mode string) *exec.Cmd {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestFetchLivenessWorkerProcess$")
	command.Dir = root
	command.Env = append(os.Environ(), "CXT_FETCH_LIVENESS_WORKER="+mode)
	t.Cleanup(func() {
		if command.Process != nil && command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	return command
}

func TestFetchLivenessObserverRestartDrainsExistingJob(t *testing.T) {
	for _, stopped := range []string{"crash", "expire"} {
		t.Run(stopped, func(t *testing.T) {
			root, source, before := fetchFixture(t, true)
			activationTestGit(t, source, "commit", "--allow-empty", "-m", "merged")
			after := activationTestGit(t, source, "rev-parse", "HEAD")
			activationTestGit(t, root, "fetch", "origin")
			resolver := &fakePRMergeResolver{pulls: []outbound.MergedPullRequest{{Number: 42, BaseBranch: "main", HeadBranch: "feature", HeadSHA: before, MergeCommitSHA: after}}}
			service := app.NewSyncRepoService(storage.NewFileStore(root), &fetchDeliveryRemote{failures: 100}, nil, storage.NewSyncOutbox())
			if pending, err := replayFetchObservations(context.Background(), &Container{PRMerges: resolver, Sync: service}, root); !pending || err != nil {
				t.Fatalf("queue preparation failed: %v %v", pending, err)
			}
			worker := fetchLivenessCommand(t, root, stopped)
			if err := worker.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(root, ".cxt/fetch-test-attempt")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("worker never attempted existing job")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if stopped == "crash" {
				if err := worker.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err := worker.Wait(); err == nil {
					t.Fatal("worker did not crash")
				}
			} else if err := worker.Wait(); err != nil {
				t.Fatal(err)
			}
			repo := string(remotecfg.RepoIDFor("https://context.example.test/team/repo"))
			jobs, err := storage.NewFileStore(root).PendingPRDeliveries(context.Background(), repo)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("worker exit lost durable intent: %+v %v", jobs, err)
			}
			activationTestGit(t, root, "fetch", "origin")
			if record := fetchRecords(t, root)[0]; record["discovery_scanned"] != true || record["delivery_state"] != "pending" {
				t.Fatalf("no-op fetch changed queued work: %v", record)
			}
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CODEX_HOME", "")
			id := "11111111-1111-4111-8111-111111111111"
			path := writeCodexRollout(t, os.Getenv("HOME"), root, id, time.Now())
			if err := capture.TrackAppSession(root, domain.ProviderCodex, id, path); err != nil {
				t.Fatal(err)
			}
			resumed := fetchLivenessCommand(t, root, "drain")
			observerCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			wakes := 0
			container := &Container{WakeFetchDiscovery: func(cwd string) {
				wakes++
				if err := spawnFetchDiscovery(observerCtx, cwd, time.Now(), resumed.Start); err != nil {
					t.Error(err)
				}
				cancel()
			}}
			if err := runGitHook(observerCtx, container, root, []string{"live-watch", "codex", id}); !errors.Is(err, context.Canceled) || wakes != 1 || resumed.Process == nil {
				t.Fatalf("observer startup did not wake queue: wakes=%d err=%v", wakes, err)
			}
			if err := resumed.Wait(); err != nil {
				t.Fatal(err)
			}
			remaining, err := storage.NewFileStore(root).PendingPRDeliveries(context.Background(), repo)
			if err != nil || len(remaining) != 0 {
				t.Fatalf("restart did not drain existing job: %+v %v", remaining, err)
			}
			if record := fetchRecords(t, root)[0]; record["cursor"] != float64(1) || record["delivery_state"] != "drained" || record["status"] != "unknown" || record["before"] != before || record["after"] != after {
				t.Fatalf("restart changed evidence or claimed proof: %v", record)
			}
			if activationTestGit(t, root, "rev-parse", "HEAD") != before {
				t.Fatal("restart moved Git selection")
			}
			if raw, err := providerfs.ReadRepoFile(root, ".cxt/HEAD"); err != nil || string(raw) != "ref: refs/heads/main\n" {
				t.Fatalf("restart moved context selection: %q %v", raw, err)
			}
		})
	}
}

func TestFetchLivenessWakeGatesAndStaleLease(t *testing.T) {
	root, source, _ := fetchFixture(t, true)
	starts := 0
	start := func() error { starts++; return nil }
	now := time.Now()
	wake := func(at time.Time) {
		t.Helper()
		if err := spawnFetchDiscovery(context.Background(), root, at, start); err != nil {
			t.Fatal(err)
		}
	}
	wake(now)
	if _, err := os.Stat(filepath.Join(root, fetchObservationDir)); !os.IsNotExist(err) || starts != 0 {
		t.Fatal("empty queue created state or spawned")
	}
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "incoming")
	activationTestGit(t, root, "fetch", "origin")
	wake(now)
	wake(now.Add(time.Second))
	if starts != 1 {
		t.Fatalf("startup lease failed to coalesce launches: %d", starts)
	}
	wake(now.Add(fetchLaunchLease))
	if starts != 2 {
		t.Fatal("stale startup lease prevented recovery")
	}
	lock, err := openFetchWorkerLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * fetchLaunchLease)
	wake(later)
	if raw, err := providerfs.ReadRepoFile(root, filepath.Join(fetchObservationDir, "wake")); err != nil || string(raw) != strconv.FormatInt(later.UnixNano(), 10) || starts != 2 {
		t.Fatalf("busy worker lost wake or duplicated launch: %q %v starts=%d", raw, err, starts)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := runFetchDiscoveryWorker(context.Background(), &Container{PRMerges: &fakePRMergeResolver{}, Sync: &fetchQueueFixture{}}, root); err != nil {
		t.Fatal(err)
	}
	wake(later.Add(fetchLaunchLease))
	if starts != 2 {
		t.Fatal("drained observation spawned another worker")
	}
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "new incoming")
	activationTestGit(t, root, "fetch", "origin")
	wake(now.Add(time.Second))
	if starts != 3 {
		t.Fatal("finished worker left a lease suppressing new work")
	}
}

func TestFetchLivenessUnconnectedWakeDoesNotCreateDirectories(t *testing.T) {
	root, _, _ := fetchFixture(t, false)
	if err := spawnFetchDiscovery(context.Background(), root, time.Now(), func() error {
		t.Fatal("unconnected wake spawned worker")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".cxt")); !os.IsNotExist(err) {
		t.Fatal("unconnected wake created context directory")
	}
}

func TestFetchLivenessInstalledHookNoOpFetchDoesNotWake(t *testing.T) {
	root, source, _ := fetchFixture(t, true)
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "incoming")
	activationTestGit(t, root, "fetch", "origin")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	log := filepath.Join(t.TempDir(), "calls")
	wrapper := filepath.Join(t.TempDir(), "cxt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + quote(log) + "\nCXT_FETCH_TEST_HELPER=1 CXT_FETCH_TEST_AUTOMATIC=1 exec " + quote(os.Args[0]) + " -test.run=^TestFetchObservationHookProcess$ -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(root, ".git/hooks/reference-transaction")
	raw, err := os.ReadFile(hook)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	replaced := false
	for index, line := range lines {
		if strings.HasPrefix(line, "CXT=") {
			lines[index] = "CXT=" + quote(wrapper)
			replaced = true
		}
	}
	if !replaced {
		t.Fatal("installed hook lacks executable binding")
	}
	if err := os.WriteFile(hook, []byte(strings.Join(lines, "\n")), 0700); err != nil {
		t.Fatal(err)
	}
	activationTestGit(t, root, "fetch", "origin")
	if raw, err := os.ReadFile(log); err == nil && strings.Contains(string(raw), "remote-ref-observe") || err != nil && !os.IsNotExist(err) {
		t.Fatalf("no-op fetch unexpectedly invoked remote observation: %q %v", raw, err)
	}
	if records := fetchRecords(t, root); len(records) != 1 || records[0]["discovery_scanned"] != false {
		t.Fatalf("no-op fetch consumed pending evidence: %v", records)
	}
}

func TestFetchLivenessWakeRechecksAuthority(t *testing.T) {
	for _, change := range []string{"git-origin", "context-origin", "disconnect", "corrupt"} {
		t.Run(change, func(t *testing.T) {
			root, source, _ := fetchFixture(t, true)
			activationTestGit(t, source, "commit", "--allow-empty", "-m", "incoming")
			activationTestGit(t, root, "fetch", "origin")
			paths, err := filepath.Glob(filepath.Join(root, fetchObservationDir, "*.json"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("missing observation: %v %v", paths, err)
			}
			switch change {
			case "git-origin":
				activationTestGit(t, root, "remote", "set-url", "origin", "https://github.com/other/repo.git")
			case "context-origin":
				err = configFixtureSave(root, remotecfg.Remotes{"origin": "https://context.example.test/other/repo"})
			case "disconnect":
				err = configFixtureSave(root, remotecfg.Remotes{})
			case "corrupt":
				err = os.WriteFile(paths[0], []byte("{"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			err = spawnFetchDiscovery(context.Background(), root, time.Now(), func() error {
				t.Fatal("unavailable authority launched worker")
				return nil
			})
			if change == "corrupt" && err == nil {
				t.Fatal("corrupt observation silently ignored")
			}
			if after, err := os.ReadFile(paths[0]); err != nil || string(after) != string(before) {
				t.Fatal("wake changed durable evidence")
			}
			if _, err := os.Stat(filepath.Join(root, fetchObservationDir, "wake")); !os.IsNotExist(err) {
				t.Fatal("unauthorized wake persisted")
			}
		})
	}
}

func TestFetchLivenessConcurrentAndInvalidLeaseWakes(t *testing.T) {
	root, source, _ := fetchFixture(t, true)
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "incoming")
	activationTestGit(t, root, "fetch", "origin")
	var starts atomic.Int32
	start := func() error { starts.Add(1); return nil }
	now := time.Now()
	results := make(chan error, 4)
	for range 4 {
		go func() {
			results <- spawnFetchDiscovery(context.Background(), root, now, start)
		}()
	}
	for range 4 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if starts.Load() != 1 {
		t.Fatalf("concurrent wakes launched %d workers", starts.Load())
	}
	for _, lease := range []string{"invalid", strconv.FormatInt(now.Add(time.Hour).UnixNano(), 10)} {
		if err := providerfs.WriteRepoFileDurable(root, filepath.Join(fetchObservationDir, "launch"), []byte(lease), 0600); err != nil {
			t.Fatal(err)
		}
		before := starts.Load()
		if err := spawnFetchDiscovery(context.Background(), root, now, start); err != nil || starts.Load() != before+1 {
			t.Fatalf("invalid/future lease prevented wake: starts=%d err=%v", starts.Load(), err)
		}
	}
	later := now.Add(fetchLaunchLease)
	failed := errors.New("start unavailable")
	if err := spawnFetchDiscovery(context.Background(), root, later, func() error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("start failure hidden: %v", err)
	}
	if err := spawnFetchDiscovery(context.Background(), root, later.Add(fetchLaunchLease), start); err != nil || starts.Load() != 4 {
		t.Fatalf("failed start permanently suppressed recovery: starts=%d err=%v", starts.Load(), err)
	}
}
