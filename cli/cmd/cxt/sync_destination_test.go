package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestNamedSyncDestinationVerifiesIdentityAndIsolatesCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	var response domain.ContentHash
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repository-connections" {
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(500)
			return
		}
		auth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(domain.RepositoryConnection{RepoID: response})
	}))
	defer server.Close()
	if err := remotecfg.Save(root, remotecfg.Remotes{"origin": "https://origin.test/acme/repo", "mirror": server.URL + "/acme/repo"}); err != nil {
		t.Fatal(err)
	}
	cfg := config{RepoRoot: root, GitDir: filepath.Join(root, ".git"), RemoteToken: "origin-private-token"}
	st, _, git := buildRepositoryAdapters(cfg)
	repo, err := git.CurrentRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	resolve := namedSyncDestination(cfg, st, git)
	if _, err := resolve(context.Background(), root, "missing"); err == nil {
		t.Fatal("unknown remote accepted")
	}
	response = domain.HashContent([]byte("unrelated repository"))
	if _, err := resolve(context.Background(), root, "mirror"); err == nil {
		t.Fatal("different repository accepted")
	}
	if auth != "" {
		t.Fatal("origin token leaked to named server")
	}
	response = repo.ID
	got, err := resolve(context.Background(), root, "mirror")
	if err != nil || got.Sync == nil || got.ApplySelectedPull == nil {
		t.Fatalf("verified destination: %+v %v", got, err)
	}
	if tokenForSyncDestination(cfg, "https://origin.test/api/v1") != cfg.RemoteToken {
		t.Fatal("origin token unavailable")
	}
	if err := authcfg.Save("origin.test", "fixture-saved-origin"); err != nil {
		t.Fatal(err)
	}
	if tokenForSyncDestination(cfg, "http://origin.test/api/v1") != "" {
		t.Fatal("HTTPS token leaked across scheme downgrade")
	}
	if err := authcfg.Save("mirror.test", "fixture-saved-mirror"); err != nil {
		t.Fatal(err)
	}
	if tokenForSyncDestination(cfg, "http://mirror.test/api/v1") != "" || tokenForSyncDestination(cfg, "https://mirror.test/api/v1") != "fixture-saved-mirror" {
		t.Fatal("saved credentials did not respect secure destination transport")
	}
}
