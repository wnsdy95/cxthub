package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/githooks"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub-cli/internal/app"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

func TestFetchObservationHookProcess(t *testing.T) {
	if os.Getenv("CXT_FETCH_TEST_HELPER") != "1" {
		return
	}
	for index, arg := range os.Args {
		if arg == "--" {
			cwd, err := os.Getwd()
			if err == nil {
				container := &Container{}
				if os.Getenv("CXT_FETCH_TEST_AUTOMATIC") == "1" {
					container.Sync = &fetchQueueFixture{root: cwd}
					container.PRMerges = &fakePRMergeResolver{pulls: []outbound.MergedPullRequest{{Number: 42, BaseBranch: "main", HeadBranch: "feature", MergeCommitSHA: gitOut(cwd, "rev-parse", "refs/remotes/origin/main")}}}
					container.WakeFetchDiscovery = func(root string) {
						cmd := exec.Command(os.Args[0], "-test.run=^TestFetchObservationHookProcess$", "--", "git-hook", "remote-ref-discovery")
						cmd.Dir = root
						cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
						if cmd.Start() == nil {
							_ = cmd.Process.Release()
						}
					}
				}
				err = runGitHook(context.Background(), container, cwd, os.Args[index+2:])
			}
			if err != nil {
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func fetchFixture(t *testing.T, connected bool) (string, string, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("CXT_REMOTE", "")
	t.Setenv("CXT_FETCH_TEST_AUTOMATIC", "")
	source, root := t.TempDir(), t.TempDir()
	activationTestGit(t, source, "init", "-b", "main")
	activationTestGit(t, source, "config", "user.name", "Fixture")
	activationTestGit(t, source, "config", "user.email", "fixture@example.test")
	activationTestGit(t, source, "config", "commit.gpgsign", "false")
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "base")
	activationTestGit(t, root, "clone", source, ".")
	activationTestGit(t, root, "remote", "set-url", "origin", "https://github.com/fixture/repo.git")
	activationTestGit(t, root, "config", "url."+source+".insteadOf", "https://github.com/fixture/repo.git")
	if connected {
		if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := configFixtureSave(root, remotecfg.Remotes{"origin": "https://context.example.test/team/repo"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := githooks.Install(root); err != nil {
		t.Fatal(err)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "cxt")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nCXT_FETCH_TEST_HELPER=1 exec "+quote(bin)+" -test.run=^TestFetchObservationHookProcess$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(root, ".git", "hooks", "reference-transaction")
	raw, err := os.ReadFile(hook)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte(strings.ReplaceAll(string(raw), quote(bin), quote(wrapper))), 0700); err != nil {
		t.Fatal(err)
	}
	return root, source, activationTestGit(t, root, "rev-parse", "HEAD")
}

func fetchRecords(t *testing.T, root string) []map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, ".cxt", "fetch-observations", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func TestFetchObservationOrdinaryGitFetchKeepsSelection(t *testing.T) {
	root, source, before := fetchFixture(t, true)
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "fetched")
	after := activationTestGit(t, source, "rev-parse", "HEAD")
	activationTestGit(t, root, "fetch", "origin")
	records := fetchRecords(t, root)
	if len(records) != 1 {
		t.Fatalf("ordinary fetch recorded %d observations, want 1", len(records))
	}
	for key, want := range map[string]string{
		"before": before, "after": after, "ref": "refs/remotes/origin/main",
		"origin": "https://github.com/fixture/repo.git", "git_repository": "github.com/fixture/repo",
		"context_origin":     "https://context.example.test/team/repo",
		"context_repository": string(remotecfg.RepoIDFor("https://context.example.test/team/repo")),
	} {
		if records[0][key] != want {
			t.Errorf("%s = %v, want %s", key, records[0][key], want)
		}
	}
	if got := activationTestGit(t, root, "rev-parse", "HEAD"); got != before {
		t.Fatalf("fetch moved local HEAD: %s", got)
	}
	if raw, err := os.ReadFile(filepath.Join(root, ".cxt", "HEAD")); err != nil || string(raw) != "ref: refs/heads/main\n" {
		t.Fatalf("context selection changed: %q %v", raw, err)
	}
	activationTestGit(t, root, "fetch", "origin")
	if got := len(fetchRecords(t, root)); got != 1 {
		t.Fatalf("no-op fetch duplicated observation: %d", got)
	}
	activationTestGit(t, root, "update-ref", "ORIG_HEAD", after)
	if got := fetchRecords(t, root)[0]["before"]; got != before {
		t.Fatalf("ORIG_HEAD replaced durable before: %v", got)
	}
}

func TestFetchObservationDisconnectedAndFailedFetch(t *testing.T) {
	for _, connected := range []bool{false, true} {
		t.Run(map[bool]string{false: "disconnected", true: "network-failure"}[connected], func(t *testing.T) {
			root, source, _ := fetchFixture(t, connected)
			activationTestGit(t, source, "commit", "--allow-empty", "-m", "next")
			if connected {
				activationTestGit(t, root, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing"))
				if err := exec.Command("git", "-C", root, "fetch", "origin").Run(); err == nil {
					t.Fatal("fetch unexpectedly succeeded")
				}
			} else {
				activationTestGit(t, root, "fetch", "origin")
			}
			if got := len(fetchRecords(t, root)); got != 0 {
				t.Fatalf("recorded %d observations without committed connected refs", got)
			}
		})
	}
}

type fetchQueueFixture struct {
	fixedBriefingSync
	err    error
	pulls  []outbound.MergedPullRequest
	inputs []inbound.SyncInput
	root   string
}

func (queue *fetchQueueFixture) QueuePullRequest(_ context.Context, input inbound.SyncInput, pull outbound.MergedPullRequest) error {
	if queue.err != nil {
		return queue.err
	}
	queue.pulls = append(queue.pulls, pull)
	queue.inputs = append(queue.inputs, input)
	if queue.root != "" {
		raw, _ := json.Marshal(map[string]string{"RepoID": input.RepoID, "Cwd": input.Cwd})
		return providerfs.WriteRepoFileDurable(queue.root, ".cxt/fetch-test-handoff", raw, 0600)
	}
	return nil
}

func (*fetchQueueFixture) ReplayPRDeliveries(context.Context, inbound.SyncInput) (bool, error) {
	return false, nil
}

func fetchObserve(t *testing.T, root, before, after, ref string) {
	t.Helper()
	if err := observeFetchedRefs(context.Background(), root, strings.NewReader(before+" "+after+" "+ref+"\n")); err != nil {
		t.Fatal(err)
	}
}

func TestFetchObservationReplayFailureAndRecovery(t *testing.T) {
	for _, failure := range []string{"provider", "handoff"} {
		t.Run(failure, func(t *testing.T) {
			root, source, before := fetchFixture(t, true)
			activationTestGit(t, source, "commit", "--allow-empty", "-m", "merge")
			after := activationTestGit(t, source, "rev-parse", "HEAD")
			activationTestGit(t, root, "fetch", "origin")
			resolver := &fakePRMergeResolver{pulls: []outbound.MergedPullRequest{{Number: 42, BaseBranch: "main", HeadBranch: "feature", MergeCommitSHA: after}}}
			queue := &fetchQueueFixture{}
			if failure == "provider" {
				resolver.err = errors.New("network unavailable")
			} else {
				queue.err = errors.New("queue disk unavailable")
			}
			container := &Container{PRMerges: resolver, Sync: queue}
			if pending, err := replayFetchObservations(context.Background(), container, root); !pending || err == nil {
				t.Fatalf("failure lost retry: pending=%v err=%v", pending, err)
			}
			record := fetchRecords(t, root)[0]
			if record["cursor"] != float64(0) || record["discovery_scanned"] != false || record["status"] != "unknown" {
				t.Fatalf("failed discovery advanced: %v", record)
			}
			paths, _ := filepath.Glob(filepath.Join(root, ".cxt", "pr-discovery", "*.json"))
			if len(paths) != 1 {
				t.Fatalf("discovery queue lost evidence: %v", paths)
			}
			resolver.err, queue.err = nil, nil
			replayPRDiscovery(context.Background(), resolver, queue, root, "main", "https://github.com/fixture/repo.git", nil)
			if len(queue.pulls) != 0 {
				t.Fatal("unscoped replay consumed fetch evidence")
			}
			if pending, err := replayFetchObservations(context.Background(), container, root); pending || err != nil {
				t.Fatalf("recovery failed: pending=%v err=%v", pending, err)
			}
			if len(queue.pulls) != 1 || queue.inputs[0].RepoID != string(remotecfg.RepoIDFor("https://context.example.test/team/repo")) || resolver.base != "main" || strings.Join(resolver.shas, ",") != after {
				t.Fatalf("wrong scoped handoff: %+v %+v %+v", queue.pulls, queue.inputs, resolver)
			}
			record = fetchRecords(t, root)[0]
			if record["cursor"] != float64(1) || record["discovery_scanned"] != true || record["status"] != "unknown" || record["before"] != before {
				t.Fatalf("local evidence lost or misrepresented: %v", record)
			}
			fetchObserve(t, root, before, after, "refs/remotes/origin/main")
			if _, err := replayFetchObservations(context.Background(), container, root); err != nil || len(queue.pulls) != 1 {
				t.Fatalf("duplicate reset cursor: pulls=%v err=%v", queue.pulls, err)
			}
			if head := activationTestGit(t, root, "rev-parse", "HEAD"); head != before {
				t.Fatal("discovery moved worktree HEAD")
			}
		})
	}
}

func TestFetchObservationUnsupportedScopesRemainUnknown(t *testing.T) {
	for _, scope := range []string{"nonorigin", "overlapping-remotes", "unsupported-remote-refspec", "refspec", "origin-change", "context-change", "disconnect", "no-queue"} {
		t.Run(scope, func(t *testing.T) {
			root, source, before := fetchFixture(t, true)
			activationTestGit(t, source, "commit", "--allow-empty", "-m", "next")
			after := activationTestGit(t, source, "rev-parse", "HEAD")
			remote := "origin"
			if scope == "nonorigin" || scope == "overlapping-remotes" || scope == "unsupported-remote-refspec" {
				activationTestGit(t, root, "remote", "add", "upstream", source)
				if scope == "nonorigin" {
					remote = "upstream"
				} else if scope == "overlapping-remotes" {
					activationTestGit(t, root, "config", "remote.upstream.fetch", "+refs/heads/*:refs/remotes/origin/*")
				} else {
					activationTestGit(t, root, "config", "remote.upstream.fetch", "HEAD")
				}
			}
			if scope == "refspec" {
				activationTestGit(t, root, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/elsewhere/*")
			}
			activationTestGit(t, root, "fetch", remote)
			if scope == "origin-change" {
				activationTestGit(t, root, "remote", "set-url", "origin", "https://github.com/other/repo.git")
			}
			if scope == "context-change" || scope == "disconnect" {
				remotes := remotecfg.Remotes{}
				if scope == "context-change" {
					remotes["origin"] = "https://context.example.test/team/other"
				}
				if err := configFixtureSave(root, remotes); err != nil {
					t.Fatal(err)
				}
			}
			queue := &fetchQueueFixture{}
			resolver := &fakePRMergeResolver{pulls: []outbound.MergedPullRequest{{Number: 99, BaseBranch: "main", HeadBranch: "feature", MergeCommitSHA: after}}}
			container := &Container{Sync: queue, PRMerges: resolver}
			if scope == "no-queue" {
				container.Sync = fixedBriefingSync{}
			}
			_, _ = replayFetchObservations(context.Background(), container, root)
			if len(queue.pulls) != 0 || resolver.remote != "" {
				t.Fatal("ambiguous origin was used for PR handoff")
			}
			records := fetchRecords(t, root)
			if len(records) == 0 {
				t.Fatal("unsupported observation was discarded")
			}
			for _, record := range records {
				if record["status"] != "unknown" || record["discovery_scanned"] != false || record["cursor"] != float64(0) {
					t.Fatalf("ambiguous observation marked complete: %v", record)
				}
			}
			if activationTestGit(t, root, "rev-parse", "HEAD") != before {
				t.Fatal("unsupported observation moved local HEAD")
			}
		})
	}
}

func TestFetchObservationDisjointUpstreamAllowsOriginDiscovery(t *testing.T) {
	root, source, before := fetchFixture(t, true)
	activationTestGit(t, root, "remote", "add", "upstream", "https://github.com/upstream/repo.git")
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "merged")
	after := activationTestGit(t, source, "rev-parse", "HEAD")
	activationTestGit(t, root, "fetch", "origin")
	queue := &fetchQueueFixture{}
	resolver := &fakePRMergeResolver{pulls: []outbound.MergedPullRequest{{Number: 42, BaseBranch: "main", HeadBranch: "feature", MergeCommitSHA: after}}}
	if pending, err := replayFetchObservations(context.Background(), &Container{Sync: queue, PRMerges: resolver}, root); pending || err != nil {
		t.Fatalf("disjoint upstream blocked origin: %v %v", pending, err)
	}
	if len(queue.pulls) != 1 || resolver.remote != "https://github.com/fixture/repo.git" || resolver.base != "main" {
		t.Fatalf("wrong discovery scope: pulls=%v resolver=%+v", queue.pulls, resolver)
	}
	if activationTestGit(t, root, "rev-parse", "HEAD") != before {
		t.Fatal("disjoint origin discovery moved selection")
	}
}

func TestFetchObservationReplayIgnoresUnrelatedBindingEdits(t *testing.T) {
	for _, change := range []string{"config-format", "preference", "disjoint-upstream", "overlapping-upstream"} {
		t.Run(change, func(t *testing.T) {
			root, source, _ := fetchFixture(t, true)
			activationTestGit(t, source, "commit", "--allow-empty", "-m", "merged")
			after := activationTestGit(t, source, "rev-parse", "HEAD")
			activationTestGit(t, root, "fetch", "origin")
			original := fetchRecords(t, root)[0]
			switch change {
			case "config-format":
				raw, err := providerfs.ReadRepoFile(root, ".cxt/config")
				if err != nil {
					t.Fatal(err)
				}
				if err := providerfs.WriteRepoFileDurable(root, ".cxt/config", append(raw, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			case "preference":
				if err := remotecfg.SetCheckoutMode(context.Background(), root, remotecfg.CheckoutPrepare); err != nil {
					t.Fatal(err)
				}
			default:
				activationTestGit(t, root, "remote", "add", "upstream", "https://github.com/upstream/repo.git")
				if change == "overlapping-upstream" {
					activationTestGit(t, root, "config", "remote.upstream.fetch", "+refs/heads/*:refs/remotes/origin/*")
				}
			}
			queue := &fetchQueueFixture{}
			resolver := &fakePRMergeResolver{pulls: []outbound.MergedPullRequest{{Number: 42, BaseBranch: "main", HeadBranch: "feature", MergeCommitSHA: after}}}
			pending, err := replayFetchObservations(context.Background(), &Container{Sync: queue, PRMerges: resolver}, root)
			if change == "overlapping-upstream" {
				if !pending || err == nil || len(queue.pulls) != 0 || resolver.remote != "" {
					t.Fatalf("new overlapping destination accepted: %v %v %+v", pending, err, queue.pulls)
				}
			} else if pending || err != nil || len(queue.pulls) != 1 {
				t.Fatalf("benign binding edit stalled replay: %v %v %+v", pending, err, queue.pulls)
			}
			retained := fetchRecords(t, root)[0]
			if retained["config_identity"] != original["config_identity"] || len(retained["remotes"].([]any)) != 1 {
				t.Fatal("replay replaced original configuration evidence")
			}
		})
	}
}

func TestFetchDestinationOverlapClassification(t *testing.T) {
	for spec, disjoint := range map[string]bool{
		"+refs/heads/*:refs/remotes/upstream/*":     true,
		"+refs/heads/*:refs/remotes/origin/*":       false,
		"refs/heads/main:refs/remotes/origin/main":  false,
		"refs/heads/main:refs/remotes/origin/other": true,
		"+refs/heads/*:refs/remotes/*/main":         false,
		"^refs/heads/private/*":                     true,
		"HEAD":                                      false,
		"refs/heads/*:refs/remotes/origin/main":     false,
		"refs/heads/*:refs/remotes/upstream/**":     false,
		"+refs/heads/*:refs/remotes/upstream/../*":  false,
	} {
		if got := fetchDestinationDisjoint(spec, "refs/remotes/origin/main"); got != disjoint {
			t.Errorf("%s: disjoint=%v want %v", spec, got, disjoint)
		}
	}
}

func TestFetchObservationMissingAndShallowHistoryKeepsCursor(t *testing.T) {
	for _, scope := range []string{"missing-before", "missing-after", "shallow"} {
		t.Run(scope, func(t *testing.T) {
			root, source, before := fetchFixture(t, true)
			activationTestGit(t, source, "commit", "--allow-empty", "-m", "next")
			after := activationTestGit(t, source, "rev-parse", "HEAD")
			activationTestGit(t, root, "-c", "core.hooksPath=/dev/null", "fetch", "origin")
			if scope == "missing-before" {
				before = strings.Repeat("a", 40)
			} else if scope == "missing-after" {
				after = strings.Repeat("b", 40)
			} else if err := os.WriteFile(filepath.Join(root, ".git", "shallow"), []byte(after+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			fetchObserve(t, root, before, after, "refs/remotes/origin/main")
			resolver := &fakePRMergeResolver{}
			if pending, err := replayFetchObservations(context.Background(), &Container{PRMerges: resolver, Sync: &fetchQueueFixture{}}, root); !pending || err == nil {
				t.Fatalf("partial history marked resolved: %v %v", pending, err)
			}
			if record := fetchRecords(t, root)[0]; record["cursor"] != float64(0) || record["discovery_scanned"] != false || resolver.remote != "" {
				t.Fatalf("partial history consumed: %v resolver=%+v", record, resolver)
			}
		})
	}
}

func TestFetchObservationOutOfOrderConcurrentDuplicateAndCorruption(t *testing.T) {
	root, source, before := fetchFixture(t, true)
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "middle")
	middle := activationTestGit(t, source, "rev-parse", "HEAD")
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "last")
	after := activationTestGit(t, source, "rev-parse", "HEAD")
	activationTestGit(t, root, "-c", "core.hooksPath=/dev/null", "fetch", "origin")
	fetchObserve(t, root, middle, after, "refs/remotes/origin/main")
	var workers sync.WaitGroup
	for count := 0; count < 6; count++ {
		workers.Go(func() {
			if err := observeFetchedRefs(context.Background(), root, strings.NewReader(before+" "+middle+" refs/remotes/origin/main\n")); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if records := fetchRecords(t, root); len(records) != 2 {
		t.Fatalf("duplicate observations: %d", len(records))
	}
	corrupt := filepath.Join(fetchObservationDir, "corrupt.json")
	if err := providerfs.WriteRepoFileDurable(root, corrupt, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	resolver := &failingRangeResolver{calls: map[string]bool{}}
	if _, err := replayFetchObservations(context.Background(), &Container{PRMerges: resolver, Sync: &fetchQueueFixture{}}, root); err == nil {
		t.Fatal("corrupt journal silently ignored")
	}
	if !resolver.calls[middle] || !resolver.calls[after] || len(resolver.calls) != 2 {
		t.Fatalf("out-of-order evidence lost: %v", resolver.calls)
	}
	if raw, err := providerfs.ReadRepoFile(root, corrupt); err != nil || string(raw) != "{" {
		t.Fatal("corrupt evidence was deleted or replaced")
	}
}

func TestFetchObservationRejectsPreparedAndAborted(t *testing.T) {
	root, _, before := fetchFixture(t, true)
	for _, phase := range []string{"prepared", "aborted"} {
		cmd := exec.Command(filepath.Join(root, ".git", "hooks", "reference-transaction"), phase)
		cmd.Dir = root
		cmd.Stdin = strings.NewReader(fmt.Sprintf("%s %s refs/remotes/origin/main\n", before, strings.Repeat("a", 40)))
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %s %v", phase, raw, err)
		}
	}
	if len(fetchRecords(t, root)) != 0 {
		t.Fatal("uncommitted refs were recorded")
	}
}

func TestFetchObservationOrdinaryFetchAutomaticallyWakesDiscovery(t *testing.T) {
	root, source, before := fetchFixture(t, true)
	t.Setenv("CXT_FETCH_TEST_AUTOMATIC", "1")
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "merge")
	activationTestGit(t, root, "fetch", "origin")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		records := fetchRecords(t, root)
		if len(records) == 1 && records[0]["discovery_scanned"] == true {
			raw, err := providerfs.ReadRepoFile(root, ".cxt/fetch-test-handoff")
			var input inbound.SyncInput
			if err != nil || json.Unmarshal(raw, &input) != nil || input.RepoID != string(remotecfg.RepoIDFor("https://context.example.test/team/repo")) {
				t.Fatalf("automatic handoff missing identity: %s %v", raw, err)
			}
			if activationTestGit(t, root, "rev-parse", "HEAD") != before {
				t.Fatal("background discovery moved local HEAD")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("ordinary fetch never handed off discovery in the background")
}

func TestFetchObservationForcePushAndPruneRetainEvidence(t *testing.T) {
	root, source, before := fetchFixture(t, true)
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "old head")
	old := activationTestGit(t, source, "rev-parse", "HEAD")
	activationTestGit(t, root, "fetch", "origin")
	activationTestGit(t, source, "reset", "--hard", before)
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "rewritten head")
	rewritten := activationTestGit(t, source, "rev-parse", "HEAD")
	activationTestGit(t, root, "fetch", "origin")
	found := false
	for _, record := range fetchRecords(t, root) {
		found = found || record["before"] == old && record["after"] == rewritten
	}
	if !found {
		t.Fatal("force-push observation lost historical head")
	}
	activationTestGit(t, source, "branch", "temporary")
	activationTestGit(t, root, "fetch", "origin")
	activationTestGit(t, source, "branch", "-D", "temporary")
	activationTestGit(t, root, "fetch", "--prune", "origin")
	found = false
	for _, record := range fetchRecords(t, root) {
		if record["ref"] == "refs/remotes/origin/temporary" && record["after"] == strings.Repeat("0", 40) {
			found = record["status"] == "unknown" && record["reason"] == "deleted_ref" && record["discovery_scanned"] == false
		}
	}
	if !found {
		t.Fatal("pruned ref was discarded or declared resolved")
	}
}

func TestFetchObservationWriteFailureIsFailOpenWithoutFollowingSymlink(t *testing.T) {
	root, source, _ := fetchFixture(t, true)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, fetchObservationDir)); err != nil {
		t.Fatal(err)
	}
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "next")
	output := activationTestGit(t, root, "fetch", "origin")
	if !strings.Contains(output, "fetch observation remains unknown") {
		t.Fatalf("write failure was hidden: %s", output)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("journal followed symlink: %v %v", entries, err)
	}
}

type changingFetchResolver struct {
	root string
}

func (resolver changingFetchResolver) ResolveMergedPullRequests(_ context.Context, _, branch string, shas []string) ([]outbound.MergedPullRequest, error) {
	err := exec.Command("git", "-C", resolver.root, "remote", "set-url", "origin", "https://github.com/other/repo.git").Run()
	return []outbound.MergedPullRequest{{Number: 42, BaseBranch: branch, HeadBranch: "feature", MergeCommitSHA: shas[0]}}, err
}

func TestFetchObservationBindingChangesDuringProviderRead(t *testing.T) {
	root, source, _ := fetchFixture(t, true)
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "next")
	activationTestGit(t, root, "fetch", "origin")
	queue := &fetchQueueFixture{}
	if pending, err := replayFetchObservations(context.Background(), &Container{Sync: queue, PRMerges: changingFetchResolver{root}}, root); !pending || err == nil || len(queue.pulls) != 0 {
		t.Fatalf("changed origin handed off PR: pending=%v err=%v pulls=%v", pending, err, queue.pulls)
	}
	if record := fetchRecords(t, root)[0]; record["cursor"] != float64(0) || record["discovery_scanned"] != false {
		t.Fatalf("changed binding advanced discovery: %v", record)
	}
}

func TestFetchObservationCursorSurvivesLaterBatchFailure(t *testing.T) {
	root, _, before := fetchFixture(t, true)
	activationTestGit(t, root, "config", "user.name", "Fixture")
	activationTestGit(t, root, "config", "user.email", "fixture@example.test")
	tree := activationTestGit(t, root, "rev-parse", "HEAD^{tree}")
	after := before
	for index := 0; index < 201; index++ {
		after = activationTestGit(t, root, "commit-tree", tree, "-p", after, "-m", fmt.Sprintf("commit %d", index))
	}
	fetchObserve(t, root, before, after, "refs/remotes/origin/main")
	resolver := &fakePRMergeResolver{}
	container := &Container{PRMerges: resolver, Sync: &fetchQueueFixture{}}
	if pending, err := replayFetchObservations(context.Background(), container, root); !pending || err != nil {
		t.Fatalf("first batch failed: %v %v", pending, err)
	}
	if record := fetchRecords(t, root)[0]; record["cursor"] != float64(200) || record["discovery_scanned"] != false {
		t.Fatalf("wrong first cursor: %v", record)
	}
	resolver.err = errors.New("offline")
	if pending, err := replayFetchObservations(context.Background(), container, root); !pending || err == nil {
		t.Fatalf("later failure lost retry: %v %v", pending, err)
	}
	if record := fetchRecords(t, root)[0]; record["cursor"] != float64(200) || record["discovery_scanned"] != false {
		t.Fatalf("failure changed cursor: %v", record)
	}
	resolver.err = nil
	if pending, err := replayFetchObservations(context.Background(), container, root); pending || err != nil {
		t.Fatalf("later batch recovery failed: %v %v", pending, err)
	}
	if record := fetchRecords(t, root)[0]; record["cursor"] != float64(201) || record["status"] != "unknown" {
		t.Fatalf("recovery lost cursor or claimed provider verification: %v", record)
	}
}

func TestFetchObservationCorruptDiscoveryCannotEscapeScope(t *testing.T) {
	root, source, _ := fetchFixture(t, true)
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "next")
	activationTestGit(t, root, "fetch", "origin")
	resolver := &fakePRMergeResolver{err: errors.New("offline")}
	queue := &fetchQueueFixture{}
	_, _ = replayFetchObservations(context.Background(), &Container{PRMerges: resolver, Sync: queue}, root)
	paths, _ := filepath.Glob(filepath.Join(root, ".cxt", "pr-discovery", "*.json"))
	if len(paths) != 1 {
		t.Fatalf("expected scoped queue: %v", paths)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var item prDiscovery
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	item.Observation = ""
	raw, _ = json.Marshal(item)
	if err := os.WriteFile(paths[0], raw, 0600); err != nil {
		t.Fatal(err)
	}
	resolver.remote, resolver.err = "", nil
	replayPRDiscovery(context.Background(), resolver, queue, root, item.Branch, item.Origin, nil)
	if resolver.remote != "" || len(queue.pulls) != 0 {
		t.Fatal("corrupt fetch scope escaped into ordinary PR replay")
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatal("corrupt discovery discarded")
	}
}

type fetchWakeResolver struct {
	root, before, after string
	calls               int
}

func (resolver *fetchWakeResolver) ResolveMergedPullRequests(ctx context.Context, _, _ string, _ []string) ([]outbound.MergedPullRequest, error) {
	resolver.calls++
	if resolver.calls != 1 {
		return nil, nil
	}
	if err := observeFetchedRefs(ctx, resolver.root, strings.NewReader(resolver.before+" "+resolver.after+" refs/remotes/origin/main\n")); err != nil {
		return nil, err
	}
	if err := providerfs.WriteRepoFileDurable(resolver.root, filepath.Join(fetchObservationDir, "wake"), []byte("second"), 0600); err != nil {
		return nil, err
	}
	return nil, runFetchDiscoveryWorker(ctx, &Container{}, resolver.root)
}

func TestFetchObservationWorkerRetainsWakeWhileLocked(t *testing.T) {
	root, source, before := fetchFixture(t, true)
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "middle")
	middle := activationTestGit(t, source, "rev-parse", "HEAD")
	activationTestGit(t, source, "commit", "--allow-empty", "-m", "last")
	after := activationTestGit(t, source, "rev-parse", "HEAD")
	activationTestGit(t, root, "-c", "core.hooksPath=/dev/null", "fetch", "origin")
	fetchObserve(t, root, before, middle, "refs/remotes/origin/main")
	if err := providerfs.WriteRepoFileDurable(root, filepath.Join(fetchObservationDir, "wake"), []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	resolver := &fetchWakeResolver{root: root, before: middle, after: after}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runFetchDiscoveryWorker(ctx, &Container{PRMerges: resolver, Sync: &fetchQueueFixture{}}, root); err != nil {
		t.Fatal(err)
	}
	records := fetchRecords(t, root)
	if resolver.calls != 2 || len(records) != 2 {
		t.Fatalf("wake lost at worker exit: calls=%d records=%d", resolver.calls, len(records))
	}
	for _, record := range records {
		if record["discovery_scanned"] != true || record["status"] != "unknown" {
			t.Fatalf("wake was not replayed or claimed proof: %v", record)
		}
	}
}

type fetchDeliveryRemote struct {
	outbound.RemoteSync
	failures int
	repos    []string
}

func (remote *fetchDeliveryRemote) SubmitPRPromotion(_ context.Context, repo string, _ domain.PullRequestMerge) error {
	remote.repos = append(remote.repos, repo)
	if remote.failures > 0 {
		remote.failures--
		return errors.New("server offline")
	}
	return nil
}

func TestFetchObservationProductionDeliveryRetriesAfterDiscoveryScanned(t *testing.T) {
	for _, scenario := range []string{"offline-online", "destination-changes-during-resolution"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			root, source, before := fetchFixture(t, true)
			activationTestGit(t, source, "commit", "--allow-empty", "-m", "merged")
			after := activationTestGit(t, source, "rev-parse", "HEAD")
			activationTestGit(t, root, "fetch", "origin")
			repo := string(remotecfg.RepoIDFor("https://context.example.test/team/repo"))
			store := storage.NewFileStore(root)
			remote := &fetchDeliveryRemote{failures: 100}
			service := app.NewSyncRepoService(store, remote, nil, storage.NewSyncOutbox())
			resolver := &fakePRMergeResolver{pulls: []outbound.MergedPullRequest{{Number: 42, BaseBranch: "main", HeadBranch: "feature", HeadSHA: before, MergeCommitSHA: after}}}
			container := &Container{PRMerges: resolver, ResolveSyncDestination: func(context.Context, string, string) (SyncDestination, error) {
				return SyncDestination{Sync: service}, nil
			}}
			if pending, err := replayFetchObservations(ctx, container, root); !pending || err != nil {
				t.Fatalf("locally queued network failure lost worker responsibility: pending=%v err=%v", pending, err)
			}
			record := fetchRecords(t, root)[0]
			if record["discovery_scanned"] != true || record["cursor"] != float64(1) {
				t.Fatalf("discovery did not hand off to durable delivery: %v", record)
			}
			store = storage.NewFileStore(root)
			pending, err := store.PendingPRDeliveries(ctx, repo)
			if err != nil || len(pending) != 1 {
				t.Fatalf("production queue lost unaccepted job: %+v %v", pending, err)
			}
			service = app.NewSyncRepoService(store, remote, nil, storage.NewSyncOutbox())
			resolver.remote, resolver.err = "", errors.New("must not rediscover scanned range")
			remote.repos = nil
			if scenario == "destination-changes-during-resolution" {
				container.ResolveSyncDestination = func(context.Context, string, string) (SyncDestination, error) {
					activationTestGit(t, root, "remote", "set-url", "origin", "https://github.com/other/repo.git")
					return SyncDestination{Sync: service}, nil
				}
				if pending, err := replayFetchObservations(ctx, container, root); !pending || err == nil || len(remote.repos) != 0 {
					t.Fatalf("changed binding drained delivery: %v %v %v", pending, err, remote.repos)
				}
				return
			}
			remote.failures = 1
			workerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := runFetchDiscoveryWorker(workerCtx, container, root); err != nil {
				t.Fatal(err)
			}
			pending, err = storage.NewFileStore(root).PendingPRDeliveries(ctx, repo)
			if err != nil || len(pending) != 0 || len(remote.repos) != 2 || resolver.remote != "" {
				t.Fatalf("scanned discovery failed automatic delivery retry: pending=%+v err=%v calls=%v rediscovered=%q", pending, err, remote.repos, resolver.remote)
			}
			for _, submittedRepo := range remote.repos {
				if submittedRepo != repo {
					t.Fatalf("delivery used unpinned repository: %s", submittedRepo)
				}
			}
			if record := fetchRecords(t, root)[0]; record["status"] != "unknown" || record["cursor"] != float64(1) {
				t.Fatalf("server acceptance fabricated proof or rewrote cursor: %v", record)
			}
			if activationTestGit(t, root, "rev-parse", "HEAD") != before {
				t.Fatal("delivery replay moved Git selection")
			}
			if raw, err := providerfs.ReadRepoFile(root, ".cxt/HEAD"); err != nil || string(raw) != "ref: refs/heads/main\n" {
				t.Fatalf("delivery replay moved context selection: %q %v", raw, err)
			}
		})
	}
}
