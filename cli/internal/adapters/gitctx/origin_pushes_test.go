package gitctx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// gitAt runs Git in a folder as it would run at a given second: Git dates the
// record of a push with the time it believes it is.
func gitAt(t *testing.T, dir string, second int64, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test",
		fmt.Sprintf("GIT_COMMITTER_DATE=@%d +0000", second))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A clone knows which pushes it made: Git records each one beside the branch
// of origin it moved. What a fetch brought, what was set by hand and what was
// pushed to another remote is not a push of this clone to origin.
func TestOriginPushesAreThePushesThisCloneMade(t *testing.T) {
	base := t.TempDir()
	repo, other, origin := filepath.Join(base, "repo"), filepath.Join(base, "other"), filepath.Join(base, "origin.git")
	const start = int64(1_700_000_000)
	git := func(dir string, second int64, args ...string) string {
		t.Helper()
		return gitAt(t, dir, second, "", args...)
	}
	read := func(dir string) []domain.RecordedPush {
		t.Helper()
		pushes, err := OriginPushes(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		return pushes
	}
	git(base, start, "init", "-q", "--bare", "-b", "main", origin)
	git(base, start, "init", "-q", "-b", "main", repo)
	git(repo, start, "commit", "-q", "--allow-empty", "-m", "first")
	first := git(repo, start, "rev-parse", "HEAD")
	git(repo, start, "remote", "add", "origin", origin)
	// Nothing was pushed: there is no record, and that is no error.
	if pushes := read(repo); pushes == nil || len(pushes) != 0 {
		t.Fatalf("before any push: %#v", pushes)
	}
	// Three pushes, the first one an hour before the second.
	git(repo, start+10, "push", "-q", "origin", "main")
	git(repo, start+20, "commit", "-q", "--allow-empty", "-m", "second")
	second := git(repo, start, "rev-parse", "HEAD")
	git(repo, start+3610, "push", "-q", "origin", "main")
	git(repo, start+3620, "push", "-q", "origin", "main:feature/deep/name")
	want := []domain.RecordedPush{
		{Branch: "feature/deep/name", SHA: second, AtSec: start + 3620},
		{Branch: "main", SHA: second, AtSec: start + 3610},
		{Branch: "main", SHA: first, AtSec: start + 10},
	}
	if pushes := read(repo); !reflect.DeepEqual(pushes, want) {
		t.Fatalf("the pushes of this clone, newest first:\n got %#v\nwant %#v", pushes, want)
	}

	// Another clone pushes a commit; this one fetches it. A ref of origin is
	// set by hand, and the branch is pushed to another remote.
	git(base, start+4000, "clone", "-q", origin, other)
	git(other, start+4010, "commit", "-q", "--allow-empty", "-m", "third, made elsewhere")
	third := git(other, start, "rev-parse", "HEAD")
	git(other, start+4020, "push", "-q", "origin", "main")
	git(repo, start+4030, "fetch", "-q", "origin")
	if held := git(repo, start, "rev-parse", "refs/remotes/origin/main"); held != third {
		t.Fatalf("the fetch did not bring %s: %s", third, held)
	}
	git(repo, start+4040, "update-ref", "refs/remotes/origin/by-hand", first)
	elsewhere := filepath.Join(base, "elsewhere.git")
	git(base, start, "init", "-q", "--bare", "-b", "main", elsewhere)
	git(repo, start, "remote", "add", "elsewhere", elsewhere)
	git(repo, start+4050, "push", "-q", "elsewhere", "main")
	if after := read(repo); !reflect.DeepEqual(after, want) {
		t.Fatalf("after a fetch, a ref set by hand and a push to another remote:\n got %#v\nwant %#v", after, want)
	}
	// The clone that pushed the third commit has exactly that one.
	if theirs := read(other); !reflect.DeepEqual(theirs, []domain.RecordedPush{{Branch: "main", SHA: third, AtSec: start + 4020}}) {
		t.Fatalf("the other clone's pushes: %#v", theirs)
	}
	// Outside a repository there is nothing to read, and that is said.
	if _, err := OriginPushes(context.Background(), base); err == nil {
		t.Fatal("a folder that is no repository gave pushes")
	}
}

// The newest push comes first whatever order Git lists the records in: an
// older Git lists them branch after branch.
func TestRecordedPushesAreNewestFirstWhateverOrderGitListsThem(t *testing.T) {
	const branches = "refs/remotes/origin/"
	one, two := strings.Repeat("1", 40), strings.Repeat("2", 40)
	listed := one + "\t" + branches + "alpha@{100}\tupdate by push\n" +
		two + "\t" + branches + "main@{300}\tupdate by push\n" +
		one + "\t" + branches + "main@{200}\tupdate by push\n"
	want := []domain.RecordedPush{{Branch: "main", SHA: two, AtSec: 300}, {Branch: "main", SHA: one, AtSec: 200}, {Branch: "alpha", SHA: one, AtSec: 100}}
	if pushes := recordedPushes(listed, branches); !reflect.DeepEqual(pushes, want) {
		t.Fatalf(" got %#v\nwant %#v", pushes, want)
	}
}

// Only the newest pushes are read: when a clone made more than are read, the
// oldest ones are left out.
func TestOriginPushesAreBounded(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	const older, newer = int64(1_700_000_000), int64(1_700_003_600)
	gitAt(t, base, older, "", "init", "-q", "-b", "main", repo)
	gitAt(t, repo, older, "", "commit", "-q", "--allow-empty", "-m", "first")
	sha := gitAt(t, repo, older, "", "rev-parse", "HEAD")
	// The record of a push is what Git writes beside the branch. They are
	// written here as pushes leave them, each beside a branch of its own:
	// first some at the older time, then as many as are read at the newer.
	record := func(second int64, from, to int) {
		t.Helper()
		var updates strings.Builder
		for index := from; index < to; index++ {
			fmt.Fprintf(&updates, "update refs/remotes/origin/b%03d %s\n", index, sha)
		}
		gitAt(t, repo, second, updates.String(), "update-ref", "--stdin", "-m", originPushNote)
	}
	const tooMany = 25
	record(older, 0, tooMany)
	record(newer, tooMany, tooMany+domain.RecordedPushLimit)
	pushes, err := OriginPushes(context.Background(), repo)
	if err != nil || len(pushes) != domain.RecordedPushLimit {
		t.Fatalf("%d records were written, %d read (%v)", tooMany+domain.RecordedPushLimit, len(pushes), err)
	}
	for _, push := range pushes {
		if push.AtSec != newer {
			t.Fatalf("an older push %+v was read in place of a newer one", push)
		}
	}
}
