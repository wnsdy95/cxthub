package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func activationTestGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestGitHookDoesNotCreateUnconnectedStore(t *testing.T) {
	repo := t.TempDir()
	activationTestGit(t, repo, "init", "-b", "main")

	if err := runGitHook(context.Background(), &Container{}, repo, []string{"post-commit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(repo, ".cxt")); !os.IsNotExist(err) {
		t.Fatalf("git hook created an unconnected .cxt store: %v", err)
	}
}

func TestGitHookQuarantinesDirectoryOnlyResidue(t *testing.T) {
	repo := t.TempDir()
	activationTestGit(t, repo, "init", "-b", "main")
	residue := filepath.Join(repo, ".cxt", "capture")
	if err := os.MkdirAll(residue, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(residue, "legacy.turn")
	if err := os.WriteFile(legacy, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The nil service container is intentional: directory-only residue must be
	// rejected before an event reaches any context use case.
	if err := runGitHook(context.Background(), &Container{}, repo, []string{"post-commit"}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(legacy); err != nil || string(got) != "keep me\n" {
		t.Fatalf("legacy residue was mutated: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".cxt", "HEAD")); !os.IsNotExist(err) {
		t.Fatalf("git hook promoted residue into an initialized store: %v", err)
	}
	ignore, err := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil || !strings.Contains(string(ignore), ".cxt/") {
		t.Fatalf(".gitignore was not repaired: %q, %v", ignore, err)
	}
	if got := activationTestGit(t, repo, "check-ignore", ".cxt/probe"); got == "" {
		t.Fatal("legacy .cxt residue remains visible to git")
	}
}

func TestHooksInstallRequiresInitializedStore(t *testing.T) {
	repo := t.TempDir()
	activationTestGit(t, repo, "init", "-b", "main")
	t.Chdir(repo)

	err := Run(&Container{}, []string{"cxt", "hooks", "install"})
	if err == nil || !strings.Contains(err.Error(), "run 'cxt init' first") {
		t.Fatalf("hooks install error = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(repo, ".cxt")); !os.IsNotExist(statErr) {
		t.Fatalf("hooks install created an uninitialized store: %v", statErr)
	}
}
