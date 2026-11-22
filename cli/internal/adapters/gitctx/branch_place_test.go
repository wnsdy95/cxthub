package gitctx

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// The branch and its newest commits are read from Git as it is.
func TestBranchPlaceReadsTheBranchAndItsNewestCommits(t *testing.T) {
	repo := branchIDTestRepo(t)
	ctx := context.Background()
	place := NewBranchPlace()
	first := branchIDTestGit(t, repo, "rev-parse", "HEAD")

	branch, newest, err := place.BranchPlace(ctx, repo, 50)
	if err != nil || branch != "main" || !reflect.DeepEqual(newest, []string{first}) {
		t.Fatalf("one commit: %q %v %v", branch, newest, err)
	}
	// Two more commits on a branch whose name has a slash. Whether a remote
	// branch holds a commit makes no difference to what is listed.
	branchIDTestGit(t, repo, "update-ref", "refs/remotes/origin/main", first)
	branchIDTestGit(t, repo, "checkout", "-q", "-b", "topic/parser")
	branchIDTestGit(t, repo, "commit", "--allow-empty", "-m", "second")
	second := branchIDTestGit(t, repo, "rev-parse", "HEAD")
	branchIDTestGit(t, repo, "commit", "--allow-empty", "-m", "third")
	third := branchIDTestGit(t, repo, "rev-parse", "HEAD")
	branch, newest, err = place.BranchPlace(ctx, repo, 50)
	if err != nil || branch != "topic/parser" || !reflect.DeepEqual(newest, []string{third, second, first}) {
		t.Fatalf("three commits: %q %v %v", branch, newest, err)
	}
	if _, newest, err = place.BranchPlace(ctx, repo, 2); err != nil || !reflect.DeepEqual(newest, []string{third, second}) {
		t.Fatalf("the newest two: %v %v", newest, err)
	}
	if branch, newest, err = place.BranchPlace(ctx, repo, 0); err != nil || branch != "topic/parser" || newest != nil {
		t.Fatalf("the branch alone: %q %v %v", branch, newest, err)
	}

	// A linked worktree stands on its own branch.
	linked := filepath.Join(filepath.Dir(repo), "linked")
	branchIDTestGit(t, repo, "worktree", "add", "-q", "-b", "linked", linked, first)
	if branch, newest, err = place.BranchPlace(ctx, linked, 50); err != nil || branch != "linked" || !reflect.DeepEqual(newest, []string{first}) {
		t.Fatalf("a linked worktree: %q %v %v", branch, newest, err)
	}
	// On no branch, and outside a repository, there is no branch to read.
	branchIDTestGit(t, repo, "checkout", "-q", "--detach")
	if branch, newest, err = place.BranchPlace(ctx, repo, 50); err != nil || branch != "" || newest != nil {
		t.Fatalf("on no branch: %q %v %v", branch, newest, err)
	}
	if branch, newest, err = place.BranchPlace(ctx, t.TempDir(), 50); err != nil || branch != "" || newest != nil {
		t.Fatalf("outside a repository: %q %v %v", branch, newest, err)
	}
	// A branch without a commit has a name and nothing to list.
	empty := filepath.Join(filepath.Dir(repo), "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	branchIDTestGit(t, empty, "init", "-q", "-b", "start")
	if branch, newest, err = place.BranchPlace(ctx, empty, 50); err != nil || branch != "start" || len(newest) != 0 {
		t.Fatalf("a branch without a commit: %q %v %v", branch, newest, err)
	}
}

// What a session was given is remembered beside Git's files, a file a session.
func TestBranchSummaryGivenRemembersABranchForEachSession(t *testing.T) {
	repo := branchIDTestRepo(t)
	ctx := context.Background()
	given := NewBranchSummaryGiven()
	if branch, err := given.GivenBranch(ctx, repo, domain.ProviderClaude, "session-1"); err != nil || branch != "" {
		t.Fatalf("nothing given yet: %q %v", branch, err)
	}
	if err := given.SetGivenBranch(ctx, repo, domain.ProviderClaude, "session-1", "topic/parser"); err != nil {
		t.Fatal(err)
	}
	if err := given.SetGivenBranch(ctx, repo, domain.ProviderCodex, "session-1", "main"); err != nil {
		t.Fatal(err)
	}
	if branch, err := given.GivenBranch(ctx, repo, domain.ProviderClaude, "session-1"); err != nil || branch != "topic/parser" {
		t.Fatalf("what was given: %q %v", branch, err)
	}
	if branch, err := given.GivenBranch(ctx, repo, domain.ProviderCodex, "session-1"); err != nil || branch != "main" {
		t.Fatalf("the same ID of the other tool: %q %v", branch, err)
	}
	if branch, err := given.GivenBranch(ctx, repo, domain.ProviderClaude, "session-2"); err != nil || branch != "" {
		t.Fatalf("another session: %q %v", branch, err)
	}
	if err := given.SetGivenBranch(ctx, repo, domain.ProviderClaude, "session-1", "main"); err != nil {
		t.Fatal(err)
	}
	// Every worktree shares it, and none shows it.
	linked := filepath.Join(filepath.Dir(repo), "linked")
	branchIDTestGit(t, repo, "worktree", "add", "-q", "-b", "linked", linked)
	if branch, err := given.GivenBranch(ctx, linked, domain.ProviderClaude, "session-1"); err != nil || branch != "main" {
		t.Fatalf("from a linked worktree: %q %v", branch, err)
	}
	if status := branchIDTestGit(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("the memory shows in the working tree: %s", status)
	}
	dir := filepath.Join(repo, ".git", "cxt", branchSummaryGivenDir)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("files: %v %v", entries, err)
	}

	// A session that was given nothing for a long time is forgotten the next
	// time anything is remembered.
	old := time.Now().Add(-branchSummaryGivenAge - time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "codex-session-1"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := given.SetGivenBranch(ctx, repo, domain.ProviderClaude, "session-3", "main"); err != nil {
		t.Fatal(err)
	}
	if branch, err := given.GivenBranch(ctx, repo, domain.ProviderCodex, "session-1"); err != nil || branch != "" {
		t.Fatalf("a session of long ago: %q %v", branch, err)
	}
	if branch, err := given.GivenBranch(ctx, repo, domain.ProviderClaude, "session-1"); err != nil || branch != "main" {
		t.Fatalf("a recent session: %q %v", branch, err)
	}

	// Only a session ID that can name a file, and a branch name of one line.
	for _, id := range []string{"", "..", "a/b", strings.Repeat("s", 129)} {
		if err := given.SetGivenBranch(ctx, repo, domain.ProviderClaude, id, "main"); err == nil {
			t.Fatalf("session ID %q was accepted", id)
		}
		if _, err := given.GivenBranch(ctx, repo, domain.ProviderClaude, id); err == nil {
			t.Fatalf("session ID %q was read", id)
		}
	}
	for _, branch := range []string{"", "two\nlines"} {
		if err := given.SetGivenBranch(ctx, repo, domain.ProviderClaude, "session-1", branch); err == nil {
			t.Fatalf("branch %q was accepted", branch)
		}
	}
	if err := given.SetGivenBranch(ctx, repo, domain.ProviderKind("other"), "session-1", "main"); err == nil {
		t.Fatal("an unknown tool was accepted")
	}
	if _, err := given.GivenBranch(ctx, t.TempDir(), domain.ProviderClaude, "session-1"); err == nil {
		t.Fatal("a folder that is not a repository must fail")
	}
}
