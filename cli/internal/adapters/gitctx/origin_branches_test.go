package gitctx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOriginBranchesChangeWhenThisCloneFetchedOrPushed(t *testing.T) {
	base := t.TempDir()
	repo, origin := filepath.Join(base, "repo"), filepath.Join(base, "origin.git")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	read := func() string {
		t.Helper()
		branches, err := OriginBranches(context.Background(), repo)
		if err != nil {
			t.Fatal(err)
		}
		return branches
	}
	git(base, "init", "-q", "--bare", "-b", "main", origin)
	git(base, "init", "-q", "-b", "main", repo)
	git(repo, "commit", "-q", "--allow-empty", "-m", "first")
	first := git(repo, "rev-parse", "HEAD")
	git(repo, "remote", "add", "origin", origin)
	// Nothing was pushed or fetched yet: this clone holds no branch of origin.
	if branches := read(); branches != "" {
		t.Fatalf("before any push: %q", branches)
	}
	git(repo, "push", "-q", "-u", "origin", "main")
	if branches := read(); branches != "refs/remotes/origin/main "+first+"\n" {
		t.Fatalf("after the first push: %q", branches)
	}
	// A commit that is not pushed changes nothing; the push of it does.
	git(repo, "commit", "-q", "--allow-empty", "-m", "second")
	second := git(repo, "rev-parse", "HEAD")
	if branches := read(); branches != "refs/remotes/origin/main "+first+"\n" {
		t.Fatalf("after a commit that was not pushed: %q", branches)
	}
	git(repo, "push", "-q", "origin", "main")
	// A second branch, and a branch of another remote, which is not origin's.
	git(repo, "push", "-q", "origin", "main:topic")
	git(repo, "remote", "add", "elsewhere", origin)
	git(repo, "update-ref", "refs/remotes/elsewhere/main", first)
	if branches := read(); branches != "refs/remotes/origin/main "+second+"\nrefs/remotes/origin/topic "+second+"\n" {
		t.Fatalf("after two pushes: %q", branches)
	}
	// What another clone pushed is seen once this one fetched.
	other := filepath.Join(base, "other")
	git(base, "clone", "-q", origin, other)
	git(other, "commit", "-q", "--allow-empty", "-m", "third")
	third := git(other, "rev-parse", "HEAD")
	git(other, "push", "-q", "origin", "main")
	if branches := read(); !strings.Contains(branches, "origin/main "+second) {
		t.Fatalf("before the fetch: %q", branches)
	}
	git(repo, "fetch", "-q", "origin")
	if branches := read(); !strings.Contains(branches, "origin/main "+third) {
		t.Fatalf("after the fetch: %q", branches)
	}
	// A folder that is no repository cannot be read.
	if branches, err := OriginBranches(context.Background(), t.TempDir()); err == nil || branches != "" {
		t.Fatalf("no repository: %q %v", branches, err)
	}
}
