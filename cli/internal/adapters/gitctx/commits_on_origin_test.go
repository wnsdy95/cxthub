package gitctx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitsOnOriginReadsWhatThisCloneKnowsWasPushed(t *testing.T) {
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
	git(base, "init", "-q", "--bare", "-b", "main", origin)
	git(base, "init", "-q", "-b", "main", repo)
	git(repo, "commit", "-q", "--allow-empty", "-m", "pushed")
	pushed := git(repo, "rev-parse", "HEAD")
	git(repo, "remote", "add", "origin", origin)
	git(repo, "push", "-q", "-u", "origin", "main")
	git(repo, "commit", "-q", "--allow-empty", "-m", "not pushed")
	local := git(repo, "rev-parse", "HEAD")
	// A commit that only another remote holds is not on origin.
	git(repo, "remote", "add", "elsewhere", origin)
	git(repo, "update-ref", "refs/remotes/elsewhere/main", local)

	unknown := strings.Repeat("f", 40)
	held := CommitsOnOrigin(context.Background(), repo, []string{pushed, local, pushed, unknown, "abc1234", ""})
	if len(held) != 1 || !held[pushed] {
		t.Fatalf("held: %v (pushed %s, local %s)", held, pushed, local)
	}
	// Once it is pushed, this clone knows.
	git(repo, "push", "-q", "origin", "main")
	if held := CommitsOnOrigin(context.Background(), repo, []string{local}); !held[local] {
		t.Fatalf("after the push: %v", held)
	}
	// A folder that is no repository holds nothing, and nothing fails.
	if held := CommitsOnOrigin(context.Background(), t.TempDir(), []string{pushed}); len(held) != 0 {
		t.Fatalf("no repository: %v", held)
	}
}
