package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func mainTestGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestReadCompositionDoesNotExposeCommandServices(t *testing.T) {
	c := buildReadContainer(config{RepoRoot: t.TempDir()}).clictr
	if c.Save != nil || c.Checkout != nil || c.Load != nil || c.Fork != nil || c.Sync != nil || c.History != nil || c.Memorize != nil || c.Init != nil || c.Tag != nil || c.Stash != nil || c.SettingsObjects != nil || c.WakeHistoricalSync != nil {
		t.Fatal("read composition exposes mutation services")
	}
	if c.List == nil || c.Queries == nil || c.ResolveRepo == nil {
		t.Fatal("missing read services")
	}
}

func TestReadCommandsDoNotCreateReplicaOrRegisterRepository(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	mainTestGit(t, root, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(root, ".claude", "settings.json")
	if err := os.WriteFile(settings, []byte("{\"permissions\":{}}"), 0600); err != nil {
		t.Fatal(err)
	}
	var requests []string
	var requestsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsMu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		requestsMu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("read made a write request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/fsck") {
			_, _ = w.Write([]byte(`{"total":0,"reachable":0}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/reflog") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		t.Errorf("unexpected read request: %s", r.URL.Path)
		http.Error(w, "unexpected endpoint", http.StatusNotFound)
	}))
	defer server.Close()
	t.Setenv("CXT_REMOTE", server.URL+"/api/v1")
	t.Setenv("CXT_TOKEN", "fixture")
	for _, command := range [][]string{{"log"}, {"branch"}, {"branch", "list"}, {"remote", "-v"}, {"config", "load.mode"}, {"tag"}, {"stash", "list"}, {"settings", "list"}, {"fsck"}, {"reflog"}} {
		if err := run(append([]string{"cxt"}, command...)); err != nil {
			t.Fatalf("%v: %v", command, err)
		}
		if _, err := os.Stat(filepath.Join(root, ".cxt")); !os.IsNotExist(err) {
			t.Fatalf("%v created replica: %v", command, err)
		}
	}
	requestsMu.Lock()
	defer requestsMu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("read requests: %v", requests)
	}
	if raw, err := os.ReadFile(settings); err != nil || string(raw) != "{\"permissions\":{}}" {
		t.Fatalf("settings changed: %s %v", raw, err)
	}
}

func TestRunHelpAndUsageErrorsBeforeComposition(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", filepath.Join(root, "home"))

	for _, args := range [][]string{
		{"cxt", "load", "--help"},
		{"cxt", "save", "-h"},
		{"cxt", "mcp", "--help"},
		{"cxt", "hook", "--help"},
		{"cxt", "version", "--help"},
	} {
		if err := run(args); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
	}

	for _, args := range [][]string{
		{"cxt", "load", "--unknown"},
		{"cxt", "save", "--unknown"},
		{"cxt", "mcp", "--unknown"},
		{"cxt", "hook", "--unknown"},
		{"cxt", "version", "--unknown"},
	} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "unknown flag") {
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

func TestDesktopWorktreeSaveUsesSharedContextStore(t *testing.T) {
	primary := filepath.Join(t.TempDir(), "primary")
	linked := filepath.Join(t.TempDir(), "codex-app-worktree")
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	mainTestGit(t, primary, "init", "-b", "main")
	mainTestGit(t, primary, "config", "user.name", "cxt test")
	mainTestGit(t, primary, "config", "user.email", "cxt@example.test")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	mainTestGit(t, primary, "config", "core.hooksPath", hooks)
	mainTestGit(t, primary, "config", "gc.auto", "0")
	if err := os.WriteFile(filepath.Join(primary, "tracked.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainTestGit(t, primary, "add", "tracked.txt")
	mainTestGit(t, primary, "commit", "-m", "initial")
	mainTestGit(t, primary, "worktree", "add", "-b", "app/codex-session", linked)
	primary, _ = filepath.EvalSymlinks(primary)
	linked, _ = filepath.EvalSymlinks(linked)
	t.Setenv("HOME", home)
	t.Chdir(linked)

	cfg := loadConfig()
	if cfg.RepoRoot != primary {
		t.Fatalf("config repo root=%q, want primary %q", cfg.RepoRoot, primary)
	}
	ctr := buildContainer(cfg)
	initOut, err := ctr.clictr.Init.Init(context.Background(), inbound.InitInput{Cwd: linked})
	if err != nil {
		t.Fatal(err)
	}

	sessionDir := filepath.Join(home, ".codex", "sessions", "2026", "08", "30")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const sessionID = "019desktop-worktree-session"
	session := filepath.Join(sessionDir, "rollout-2026-08-30T00-00-00-"+sessionID+".jsonl")
	raw := `{"type":"session_meta","payload":{"id":"` + sessionID + `","cwd":"` + linked + `","model":"gpt-test"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"desktop capture"}]}}` + "\n"
	if err := os.WriteFile(session, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := ctr.clictr.Save.Save(context.Background(), inbound.SaveInput{
		Cwd: linked, Provider: domain.ProviderCodex, SessionPath: session,
		Message: domain.HookMessagePrefix + "desktop capture", Pending: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Branch != "app/codex-session" || out.SessionID != sessionID {
		t.Fatalf("save output=%+v", out)
	}
	pendings, err := storage.NewFileStore(primary).ListPendings(context.Background(), initOut.RepoID)
	if err != nil || len(pendings) != 1 || pendings[0].Target != out.SnapshotID {
		t.Fatalf("shared pending=%+v err=%v", pendings, err)
	}
	if _, err := os.Stat(filepath.Join(primary, ".cxt")); err != nil {
		t.Fatalf("primary store missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(linked, ".cxt")); !os.IsNotExist(err) {
		t.Fatalf("split worktree store created: %v", err)
	}
}
