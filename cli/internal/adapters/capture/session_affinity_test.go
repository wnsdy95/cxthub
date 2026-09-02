package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestSessionAffinityIsTerminalAndProviderScoped(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_SESSION_ID", "terminal-a")
	RecordSessionAffinity(repo, domain.ProviderCodex, "11111111-1111-4111-8111-111111111111")
	if got := SessionAffinity(repo, domain.ProviderCodex); got != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("same terminal affinity = %q", got)
	}
	if got := SessionAffinity(repo, domain.ProviderClaude); got != "" {
		t.Fatalf("provider leaked affinity = %q", got)
	}
	t.Setenv("TERM_SESSION_ID", "terminal-b")
	if got := SessionAffinity(repo, domain.ProviderCodex); got != "" {
		t.Fatalf("terminal leaked affinity = %q", got)
	}
}

func TestSessionAffinityUsesSharedStoreFromLinkedWorktree(t *testing.T) {
	t.Setenv("TERM_SESSION_ID", "linked-terminal")
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
	if err := os.WriteFile(filepath.Join(primary, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const id = "44444444-4444-4444-8444-444444444444"
	RecordSessionAffinity(primary, domain.ProviderCodex, id)
	if got := SessionAffinity(linked, domain.ProviderCodex); got != id {
		t.Fatalf("linked affinity = %q, want %q", got, id)
	}
	if _, err := os.Lstat(filepath.Join(linked, ".cxt")); !os.IsNotExist(err) {
		t.Fatalf("linked worktree gained split state: %v", err)
	}
}
