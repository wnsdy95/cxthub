package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestConfigCoordinationFrozenHTTPDestinationAndCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	git("init", "--template=", "-q", "-b", "topic")
	git("remote", "add", "origin", "https://example.invalid/code/original.git")
	git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/topic")
	root, _ = filepath.EvalSymlinks(root)
	var stable string
	var requests atomic.Int32
	var registered domain.Repo
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer frozen-fixture" {
			t.Errorf("credential selection changed")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/repository-connections":
			if r.URL.Query().Get("remote_url") != strings.TrimSuffix(stable, "/context") {
				t.Errorf("unexpected requested address")
			}
			_ = json.NewEncoder(w).Encode(domain.RepositoryConnection{RepoID: remotecfg.RepoIDFor(stable), RemoteURL: stable, RepositoryID: "ws_fixture"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/"+remotecfg.RepoIDFor(stable):
			_ = json.NewEncoder(w).Encode(domain.Repo{ID: remotecfg.RepoIDFor(stable), RemoteURL: stable, DefaultBranch: "topic", ContextProtocol: 1})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos":
			if err := json.NewDecoder(r.Body).Decode(&registered); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(registered)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	stable = server.URL + "/team/repo/context"
	cfg := config{RepoRoot: root, GitDir: filepath.Join(root, ".git"), RemoteEndpoint: server.URL + "/api/v1", RemoteToken: "frozen-fixture"}
	prepared, err := prepareRemoteConnection(cfg)(context.Background(), root, server.URL+"/team/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatal("preparation registered before explicit Connect")
	}
	if _, err := os.Stat(filepath.Join(root, ".cxt")); !os.IsNotExist(err) {
		t.Fatal("preparation mutated local store")
	}
	// Neither ambient origin nor saved credentials may retarget this attempt.
	if err := configFixtureSave(root, remotecfg.Remotes{"origin": "https://example.invalid/other/repo"}); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(server.URL)
	if err := authcfg.Save(u.Host, "later-fixture"); err != nil {
		t.Fatal(err)
	}
	out, err := prepared.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Repo.ID != remotecfg.RepoIDFor(stable) || registered.RemoteURL != stable || registered.GitRemoteURL != "https://example.invalid/code/original.git" || registered.DefaultBranch != "topic" || registered.LocalPath != root {
		t.Fatalf("frozen metadata changed: %+v", registered)
	}
	if requests.Load() != 3 {
		t.Fatalf("wrong policy request count: %d", requests.Load())
	}
	if err := prepared.ValidateLocal(context.Background()); err != nil {
		t.Fatalf("config change retargeted Git evidence: %v", err)
	}
	git("remote", "set-url", "origin", "https://example.invalid/code/changed.git")
	if err := prepared.ValidateLocal(context.Background()); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatalf("Git drift not rejected: %v", err)
	}
}

func TestConfigCoordinationCredentialBoundary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := config{RemoteEndpoint: "https://origin.test/api/v1", RemoteToken: "origin-fixture"}
	if err := authcfg.Save("other.test", "other-fixture"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ target, observed, want string }{
		{"https://origin.test/api/v1", "", "origin-fixture"},
		{"https://other.test/api/v1", "", "other-fixture"},
		{"http://origin.test/api/v1", "", ""},
		{"http://other.test/api/v1", "", ""},
		{"https://origin.test/api/v1", "https://other.test/team/repo", ""},
	} {
		if got := tokenForObservedDestination(cfg, tc.target, tc.observed); got != tc.want {
			t.Fatalf("credential scope failed for %s", tc.target)
		}
	}
}
