package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func mainTestGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// Commands that only look must leave the folder as it is and ask the server nothing.
func TestCommandsThatOnlyLookCreateNothingAndAskNothing(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	mainTestGit(t, root, "init", "-q", "-b", "main")
	mainTestGit(t, root, "-c", "core.hooksPath=/dev/null", "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-qm", "fixture")
	var requests []string
	var requestsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsMu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		requestsMu.Unlock()
		http.Error(w, "unexpected endpoint", http.StatusNotFound)
	}))
	defer server.Close()
	t.Setenv("CXT_REMOTE", server.URL+"/api/v1")
	t.Setenv("CXT_TOKEN", "fixture")
	for _, command := range [][]string{{"segments", "--json"}, {"segments"}, {"log"}, {"log", "--json"}, {"remote", "-v"}, {"remote"},
		{"config", "segments.auto"}, {"config", "secrets.scrub"}, {"settings", "list"}, {"version"},
		// What a hook runs in a folder that was never set up.
		{"git-hook", "wake"}, {"git-hook", "post-commit"}, {"git-hook", "branch-transaction", "prepared", "1"}, {"git-hook", "segment-collect"}} {
		if err := run(append([]string{"cxt"}, command...)); err != nil {
			t.Fatalf("%v: %v", command, err)
		}
		if _, err := os.Stat(filepath.Join(root, ".cxt")); !os.IsNotExist(err) {
			t.Fatalf("%v created .cxt: %v", command, err)
		}
	}
	requestsMu.Lock()
	defer requestsMu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("requests: %v", requests)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
		t.Fatalf("the folder changed: %v %v", entries, err)
	}
}

func TestRunHelpAndUsageErrorsBeforeComposition(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", filepath.Join(root, "home"))

	for _, args := range [][]string{
		{"cxt", "setup", "--help"},
		{"cxt", "push", "-h"},
		{"cxt", "segments", "--help"},
		{"cxt", "doctor", "--help"},
		{"cxt", "hook", "--help"},
		{"cxt", "version", "--help"},
		{"cxt", "help", "fork"},
		{"cxt", "help"},
	} {
		if err := run(args); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
	}

	for _, args := range [][]string{
		{"cxt", "setup", "--unknown"},
		{"cxt", "push", "--unknown"},
		{"cxt", "doctor", "--unknown"},
		{"cxt", "hook", "--unknown"},
		{"cxt", "version", "--unknown"},
	} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("run(%v) error = %v", args, err)
		}
	}
	// A command of the removed capture approach is an unknown command, not a silent success.
	for _, args := range [][]string{{"cxt", "save"}, {"cxt", "checkout", "main"}, {"cxt", "claude"}, {"cxt", "mcp", "--local"}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("run(%v) error = %v", args, err)
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("help/usage preflight mutated cwd: %+v", entries)
	}
}
