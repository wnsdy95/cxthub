package gitctx

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestForkBranchesResolveACommitAndMakeABranchAtIt(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "first")
	first := git("rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "second")
	second := git("rev-parse", "HEAD")
	forks := NewForkBranches()
	ctx := context.Background()

	// A commit is named by its number, an abbreviation of it or any revision.
	for _, revision := range []string{first, first[:8], "HEAD~1", "main~1"} {
		if got, err := forks.ResolveCommit(ctx, repo, revision); err != nil || got != first {
			t.Fatalf("%s: %q %v", revision, got, err)
		}
	}
	for _, revision := range []string{strings.Repeat("f", 40), "no-such-branch", "", "--all", "HEAD~9"} {
		if _, err := forks.ResolveCommit(ctx, repo, revision); err == nil {
			t.Fatalf("%q was resolved", revision)
		}
	}

	// The branch is made at the commit, and the folder stays where it is.
	if err := forks.CreateBranchAt(ctx, repo, "topic/from-first", first); err != nil {
		t.Fatal(err)
	}
	if got := git("rev-parse", "refs/heads/topic/from-first"); got != first {
		t.Fatalf("the branch is at %s", got)
	}
	if got := git("symbolic-ref", "--short", "HEAD"); got != "main" || git("rev-parse", "HEAD") != second {
		t.Fatalf("the folder moved to %s", got)
	}
	// No upstream is set: the new branch is its own.
	if out, err := exec.Command("git", "-C", repo, "config", "--get", "branch.topic/from-first.merge").CombinedOutput(); err == nil {
		t.Fatalf("an upstream was set: %s", out)
	}
	for name, attempt := range map[string]func() error{
		"a branch that exists":             func() error { return forks.CreateBranchAt(ctx, repo, "topic/from-first", second) },
		"a name Git does not take":         func() error { return forks.CreateBranchAt(ctx, repo, "bad..name", first) },
		"a name that looks like an option": func() error { return forks.CreateBranchAt(ctx, repo, "--force", first) },
		"an abbreviated commit":            func() error { return forks.CreateBranchAt(ctx, repo, "topic/short", first[:8]) },
		"a commit Git does not hold":       func() error { return forks.CreateBranchAt(ctx, repo, "topic/unknown", strings.Repeat("f", 40)) },
	} {
		if err := attempt(); err == nil {
			t.Fatalf("%s: a branch was made", name)
		}
	}
	if got := git("rev-parse", "refs/heads/topic/from-first"); got != first {
		t.Fatalf("a refused attempt moved the branch to %s", got)
	}
	if out := git("branch", "--list"); strings.Contains(out, "topic/short") || strings.Contains(out, "topic/unknown") || strings.Contains(out, "bad") {
		t.Fatalf("a refused attempt left a branch: %s", out)
	}
}
