package gitctx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
