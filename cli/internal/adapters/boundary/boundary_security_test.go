package boundary

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

func TestLoadValidatesBoundaryState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	sessionDir := filepath.Join(home, ".claude", "projects", "repo")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(sessionDir, providerfs.NewSessionID()+".jsonl")
	if err := os.WriteFile(seed, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	superseded := seed + ".superseded"
	b := Boundary{PrevBranch: "main", Branch: "feature/auth", SeedPath: seed, SeedID: providerfs.NewSessionID(), Superseded: []string{superseded}}
	if err := Record(repo, b); err != nil {
		t.Fatal(err)
	}
	got, ok := Load(repo)
	if !ok || len(got.Superseded) != 1 || got.SeedID != b.SeedID {
		t.Fatalf("valid boundary rejected: %+v, %v", got, ok)
	}

	poisoned := got
	poisoned.At = time.Now().Add(time.Hour).Format(time.RFC3339)
	writeBoundaryFixture(t, repo, poisoned)
	if _, ok := Load(repo); ok {
		t.Fatal("future boundary timestamp was accepted")
	}

	poisoned = got
	poisoned.SeedID = "../../outside"
	writeBoundaryFixture(t, repo, poisoned)
	if _, ok := Load(repo); ok {
		t.Fatal("unsafe seed ID was accepted")
	}
}

func TestLoadFiltersOutsideSupersededPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	b := Boundary{
		At:         time.Now().UTC().Format(time.RFC3339),
		PrevBranch: "main",
		Branch:     "feature/auth",
		Superseded: []string{filepath.Join(t.TempDir(), "victim.jsonl")},
	}
	writeBoundaryFixture(t, repo, b)
	got, ok := Load(repo)
	if !ok || len(got.Superseded) != 0 {
		t.Fatalf("outside path was not filtered: %+v, %v", got, ok)
	}
}

func TestSupersedeRejectsOutsidePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim.jsonl")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Supersede(repo, outside); got != "" {
		t.Fatalf("outside path superseded as %q", got)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatalf("outside file changed: %q, %v", data, err)
	}
}

func TestRestoreSupersededRollsBackFileAndLedger(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	sessionDir := filepath.Join(home, ".claude", "projects", "repo")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(sessionDir, providerfs.NewSessionID()+".jsonl")
	if err := os.WriteFile(original, []byte("session\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	renamed := Supersede(repo, original)
	if renamed == "" || !providerfs.CaptureExcluded(repo, original, 999) {
		t.Fatal("session was not superseded")
	}
	if !RestoreSuperseded(repo, renamed) {
		t.Fatal("superseded session was not restored")
	}
	if data, err := os.ReadFile(original); err != nil || string(data) != "session\n" {
		t.Fatalf("restored file = %q, err=%v", data, err)
	}
	if providerfs.CaptureExcluded(repo, original, 999) {
		t.Fatal("rollback left the session excluded in the ledger")
	}
}

func TestLinkedWorktreesShareStoreButKeepIndependentBoundaries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	primary := filepath.Join(t.TempDir(), "primary")
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.MkdirAll(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(cwd string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(primary, "init", "-b", "main")
	git(primary, "config", "user.name", "cxt test")
	git(primary, "config", "user.email", "cxt@example.test")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	git(primary, "config", "core.hooksPath", hooks)
	git(primary, "config", "gc.auto", "0")
	git(primary, "commit", "--allow-empty", "-m", "initial")
	git(primary, "worktree", "add", "-b", "feature/app", linked)
	if err := os.Mkdir(filepath.Join(primary, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Record(primary, Boundary{PrevBranch: "feature/app", Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := Record(linked, Boundary{PrevBranch: "main", Branch: "feature/app"}); err != nil {
		t.Fatal(err)
	}
	mainBoundary, mainOK := Load(primary)
	linkedBoundary, linkedOK := Load(linked)
	if !mainOK || mainBoundary.Branch != "main" {
		t.Fatalf("primary boundary = %+v, %v", mainBoundary, mainOK)
	}
	if !linkedOK || linkedBoundary.Branch != "feature/app" {
		t.Fatalf("linked boundary = %+v, %v", linkedBoundary, linkedOK)
	}
	if _, err := os.Lstat(filepath.Join(linked, ".cxt")); !os.IsNotExist(err) {
		t.Fatalf("linked worktree gained a split .cxt store: %v", err)
	}
	realPrimary, err := filepath.EvalSymlinks(primary)
	if err != nil {
		t.Fatal(err)
	}
	if path := boundaryPath(linked); filepath.Dir(filepath.Dir(path)) != filepath.Join(realPrimary, ".cxt") {
		t.Fatalf("linked boundary path = %q, want shared .cxt/boundaries", path)
	}
}

func writeBoundaryFixture(t *testing.T, repo string, b Boundary) {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := providerfs.WriteRepoFileAtomic(repo, filepath.Join(".cxt", "boundary.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
