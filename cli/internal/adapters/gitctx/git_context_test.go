package gitctx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGitContextTestCommand(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=cxt test",
		"GIT_AUTHOR_EMAIL=cxt@example.invalid",
		"GIT_COMMITTER_NAME=cxt test",
		"GIT_COMMITTER_EMAIL=cxt@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestLocalBranchesListsAllHeadsWhileDetached(t *testing.T) {
	repo := t.TempDir()
	runGitContextTestCommand(t, repo, "init", "-q")
	runGitContextTestCommand(t, repo, "symbolic-ref", "HEAD", "refs/heads/main")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitContextTestCommand(t, repo, "add", "tracked.txt")
	runGitContextTestCommand(t, repo, "commit", "-q", "-m", "base")
	runGitContextTestCommand(t, repo, "branch", "feature/nested")
	runGitContextTestCommand(t, repo, "branch", "release")
	runGitContextTestCommand(t, repo, "checkout", "-q", "--detach", "HEAD")

	branches, err := NewGitContextAdapter().LocalBranches(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(branches))
	for _, branch := range branches {
		got[branch] = true
	}
	for _, want := range []string{"main", "feature/nested", "release"} {
		if !got[want] {
			t.Fatalf("LocalBranches() = %v, missing %q", branches, want)
		}
	}
	if len(got) != 3 {
		t.Fatalf("LocalBranches() = %v, want exactly 3 heads", branches)
	}
}

func TestLocalBranchesFailsOutsideGitRepository(t *testing.T) {
	if _, err := NewGitContextAdapter().LocalBranches(context.Background(), t.TempDir()); err == nil {
		t.Fatal("LocalBranches() outside a Git repository succeeded")
	}
}

func TestCurrentCommitUsesTargetWorktreeAndDetachedHEAD(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	run := func(cwd string, args ...string) string {
		t.Helper()
		command := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-C", cwd}, args...)
		cmd := exec.Command("git", command...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(root, "init", "-q")
	run(root, "commit", "--allow-empty", "-qm", "base")
	base := run(root, "rev-parse", "HEAD")
	second := filepath.Join(t.TempDir(), "worker")
	run(root, "worktree", "add", "-qb", "worker", second)
	run(second, "commit", "--allow-empty", "-qm", "worker change")
	worker := run(second, "rev-parse", "HEAD")
	a := NewGitContextAdapter()
	if got, err := a.CurrentCommit(ctx, root); err != nil || got != base {
		t.Fatal(got, err)
	}
	if got, err := a.CurrentCommit(ctx, second); err != nil || got != worker || got == base {
		t.Fatal(got, err)
	}
	// Hooks export these variables. They must not override an explicit target
	// worktree when cxt restores another directory.
	t.Setenv("GIT_DIR", filepath.Join(root, ".git"))
	t.Setenv("GIT_WORK_TREE", root)
	if got, err := a.CurrentCommit(ctx, second); err != nil || got != worker {
		t.Fatalf("inherited hook environment selected another worktree: %s %v", got, err)
	}
	os.Unsetenv("GIT_DIR")
	os.Unsetenv("GIT_WORK_TREE")
	run(second, "checkout", "-q", "--detach", base)
	if got, err := a.CurrentCommit(ctx, second); err != nil || got != base {
		t.Fatal(got, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := a.CurrentCommit(canceled, second); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
