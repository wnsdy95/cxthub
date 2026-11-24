package gitctx

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestValidRemoteName(t *testing.T) {
	for _, name := range []string{"origin", "upstream", "team/main", "my-fork_2", "a.b"} {
		if !ValidRemoteName(name) {
			t.Errorf("%q is a remote name", name)
		}
	}
	for _, name := range []string{"", "-origin", "/origin", "origin/", "a//b", "a..b", "two words", "tab\tname", "line\nbreak", "star*", "what?", "set[a]", `back\slash`, "til~de", "car^et", "co:lon", "del\x7f"} {
		if ValidRemoteName(name) {
			t.Errorf("%q was taken as a remote name", name)
		}
	}
}

// A repository whose Git remote is not named origin: cxt follows the remote
// it was told, and everything it reads of a remote is read of that one.
func TestFollowedRemoteIsTheOneEveryReadUses(t *testing.T) {
	base := t.TempDir()
	repo, upstreamDir, forkDir := filepath.Join(base, "repo"), filepath.Join(base, "upstream.git"), filepath.Join(base, "fork.git")
	const start = int64(1_700_000_000)
	ctx := context.Background()
	git := func(dir string, second int64, args ...string) string {
		t.Helper()
		return gitAt(t, dir, second, "", args...)
	}
	git(base, start, "init", "-q", "--bare", "-b", "main", upstreamDir)
	git(base, start, "init", "-q", "--bare", "-b", "main", forkDir)
	git(base, start, "init", "-q", "-b", "main", repo)
	git(repo, start, "commit", "-q", "--allow-empty", "-m", "first")
	first := git(repo, start, "rev-parse", "HEAD")

	// No remote at all: origin is the name, and there is no such remote.
	if name := FollowedRemote(ctx, repo); name != "origin" {
		t.Fatalf("with nothing set cxt follows %q", name)
	}
	if name, exists := FollowOnlyRemote(ctx, repo); name != "origin" || exists {
		t.Fatalf("a repository without a remote: %q %v", name, exists)
	}
	// Its only remote is named upstream: setup follows it without being told.
	git(repo, start, "remote", "add", "upstream", "https://git.example.test/team/app.git")
	if name, exists := FollowOnlyRemote(ctx, repo); name != "upstream" || !exists {
		t.Fatalf("the only remote of the repository: %q %v", name, exists)
	}
	if held := git(repo, start, "config", "--get", FollowedRemoteSetting); held != "upstream" {
		t.Fatalf("the name is kept in Git's configuration as %q", held)
	}
	// The address of the followed remote names the repository.
	adapter := NewGitContextAdapter()
	current, err := adapter.CurrentRepo(ctx, repo)
	if err != nil || current.GitRemoteURL != "https://git.example.test/team/app.git" {
		t.Fatalf("the repository is named by %q (%v)", current.GitRemoteURL, err)
	}
	named := current.ID

	// A second remote is added and pushed to; the followed one stays.
	git(repo, start, "remote", "set-url", "upstream", upstreamDir)
	git(repo, start, "remote", "add", "fork", forkDir)
	git(repo, start+10, "push", "-q", "upstream", "main")
	git(repo, start+20, "push", "-q", "fork", "main:other")
	if name, exists := FollowOnlyRemote(ctx, repo); name != "upstream" || !exists {
		t.Fatalf("after a second remote was added: %q %v", name, exists)
	}
	branches, err := OriginBranches(ctx, repo)
	if err != nil || branches != "refs/remotes/upstream/main "+first+"\n" {
		t.Fatalf("the branches of the followed remote: %q %v", branches, err)
	}
	pushes, err := OriginPushes(ctx, repo)
	if err != nil || !reflect.DeepEqual(pushes, []domain.RecordedPush{{Branch: "main", SHA: first, AtSec: start + 10}}) {
		t.Fatalf("the pushes to the followed remote: %#v %v", pushes, err)
	}
	git(repo, start+30, "commit", "-q", "--allow-empty", "-m", "second, not pushed")
	second := git(repo, start, "rev-parse", "HEAD")
	if held := CommitsOnOrigin(ctx, repo, []string{first, second}); !held[first] || held[second] {
		t.Fatalf("what the followed remote holds: %v", held)
	}
	git(repo, start+40, "branch", "--set-upstream-to=upstream/main", "main")
	local, err := NewBranchIDs().LocalBranches(ctx, repo)
	if err != nil || len(local) != 1 || local[0].Name != "main" || local[0].Remote != "main" {
		t.Fatalf("the local branch and the followed remote: %#v %v", local, err)
	}

	// Told another remote: every read follows.
	if err := FollowRemote(ctx, repo, "fork"); err != nil {
		t.Fatal(err)
	}
	if branches, err := OriginBranches(ctx, repo); err != nil || branches != "refs/remotes/fork/other "+first+"\n" {
		t.Fatalf("the branches after following the fork: %q %v", branches, err)
	}
	if pushes, err := OriginPushes(ctx, repo); err != nil || len(pushes) != 1 || pushes[0].Branch != "other" || pushes[0].AtSec != start+20 {
		t.Fatalf("the pushes after following the fork: %#v %v", pushes, err)
	}
	git(repo, start, "remote", "set-url", "fork", "https://git.example.test/me/fork.git")
	if moved, err := adapter.CurrentRepo(ctx, repo); err != nil || moved.ID == named || moved.GitRemoteURL != "https://git.example.test/me/fork.git" {
		t.Fatalf("the repository after following the fork: %+v %v", moved, err)
	}
	// The records a session is cut by name the same remote.
	if records, _, err := NewSessionGitRecords().ReadGitRecords(ctx, repo); err != nil || records.Origin != NormalizeRemoteURL("https://git.example.test/me/fork.git") {
		t.Fatalf("the Git records name the remote %q (%v)", records.Origin, err)
	}
	// What is no remote of the repository is refused, and nothing changes.
	for _, name := range []string{"origin", "", "up stream", "fork*"} {
		if err := FollowRemote(ctx, repo, name); err == nil || !strings.Contains(err.Error(), "upstream") {
			t.Errorf("following %q: %v", name, err)
		}
	}
	if name := FollowedRemote(ctx, repo); name != "fork" {
		t.Fatalf("after refused names cxt follows %q", name)
	}
	// A setting that is no remote name is read as none.
	git(repo, start, "config", FollowedRemoteSetting, "fork*")
	if name := FollowedRemote(ctx, repo); name != "origin" {
		t.Fatalf("a setting that is no remote name is followed as %q", name)
	}

	// A repository with two remotes, none of them origin and none chosen:
	// setup chooses none, and says that there is no remote to follow.
	two := filepath.Join(base, "two")
	git(base, start, "init", "-q", "-b", "main", two)
	git(two, start, "remote", "add", "alpha", upstreamDir)
	git(two, start, "remote", "add", "beta", forkDir)
	if name, exists := FollowOnlyRemote(ctx, two); name != "origin" || exists {
		t.Fatalf("two remotes and no origin: %q %v", name, exists)
	}
	if _, err := gitRead(ctx, two, nil, "config", "--get", FollowedRemoteSetting); err == nil {
		t.Fatal("a remote was chosen among two")
	}
	// What was set stays, also when that remote is gone and one other is
	// left: setup then says that there is no remote of that name.
	git(two, start, "remote", "remove", "beta")
	git(two, start, "config", FollowedRemoteSetting, "beta")
	if name, exists := FollowOnlyRemote(ctx, two); name != "beta" || exists {
		t.Fatalf("a followed remote that was removed: %q %v", name, exists)
	}
	git(two, start, "config", "--unset", FollowedRemoteSetting)
	git(two, start, "remote", "add", "beta", forkDir)
	// A repository with origin and one more: origin, and nothing is written.
	git(two, start, "remote", "rename", "alpha", "origin")
	git(two, start, "remote", "remove", "beta")
	if name, exists := FollowOnlyRemote(ctx, two); name != "origin" || !exists {
		t.Fatalf("a repository whose only remote is origin: %q %v", name, exists)
	}
	if _, err := gitRead(ctx, two, nil, "config", "--get", FollowedRemoteSetting); err == nil {
		t.Fatal("origin was written although it is what is followed anyway")
	}
	// Outside a repository there is no remote to follow, and that is an error for a name that was given.
	if err := FollowRemote(ctx, base, "origin"); err == nil {
		t.Fatal("a remote was followed outside a repository")
	}
	if name, exists := FollowOnlyRemote(ctx, base); name != "origin" || exists {
		t.Fatalf("outside a repository: %q %v", name, exists)
	}
}
