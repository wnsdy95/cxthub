package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
)

func bindRepairFixture(t *testing.T, remote string) string {
	t.Helper()
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	mainTestGit(t, root, "init", "-q", "-b", "main")
	j, err := branchjournal.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Bind(string(remotecfg.RepoIDFor(remote))); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRepairFromServerInlineRemoteReachesSelectedEndpoint(t *testing.T) {
	for _, inline := range []bool{false, true} {
		name := "spaced"
		if inline {
			name = "inline"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, "access denied", http.StatusForbidden)
			}))
			defer server.Close()
			remote := server.URL + "/team/repository"
			bindRepairFixture(t, remote)
			t.Setenv("CXT_TOKEN", "fixture-token")
			args := []string{"cxt", "repair", "--from-server", "--remote", remote}
			if inline {
				args = []string{"cxt", "repair", "--from-server", "--remote=" + remote}
			}
			err := run(args)
			var response *backendclient.HTTPError
			if !errors.As(err, &response) || response.StatusCode() != http.StatusForbidden || calls.Load() == 0 {
				t.Fatalf("explicit endpoint ignored: %v requests=%d", err, calls.Load())
			}
		})
	}
}

func TestRepairInlineRemoteCannotFallBackToConfiguredOrigin(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected request", http.StatusForbidden)
	}))
	defer server.Close()
	origin := server.URL + "/team/repository"
	root := bindRepairFixture(t, origin)
	if err := remotecfg.Save(root, remotecfg.Remotes{"origin": origin}); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"cxt", "repair", "--from-server", "--remote=" + server.URL + "/team/other"})
	if err == nil || !strings.Contains(err.Error(), "repair URL conflicts with durable repository identity") || calls.Load() != 0 {
		t.Fatalf("ignored conflicting explicit remote: %v requests=%d", err, calls.Load())
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "cxt", "repairs")); !os.IsNotExist(err) {
		t.Fatalf("identity rejection started repair: %v", err)
	}
}

func TestForcedPullAndInvalidRepairStopBeforeComposition(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	for _, args := range [][]string{{"cxt", "pull", "--force"}, {"cxt", "pull", "-f"}, {"cxt", "repair", "--from-server", "--remote=https://example.test/a/b", "--remote", "https://example.test/a/b"}} {
		err := run(args)
		if err == nil {
			t.Fatalf("invalid command accepted: %v", args)
		}
		failure := delivcli.ClassifyCommandFailure(err)
		if failure.Code != "invalid_arguments" || failure.State != "unchanged" || failure.ExitCode != 2 {
			t.Fatalf("reached composition: %v %+v", args, failure)
		}
	}
	// The isolated composition root must use the same registry even when called directly.
	if err := runRepair([]string{"--from-server", "--remote="}); err == nil || delivcli.ClassifyCommandFailure(err).Code != "invalid_arguments" {
		t.Fatalf("direct repair skipped preflight: %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("invalid commands touched the working directory: %v %v", entries, err)
	}
}
