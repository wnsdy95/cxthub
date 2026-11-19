package gitctx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestHeadLogMadeCommit(t *testing.T) {
	for message, want := range map[string]bool{
		"commit: add the reader":                                true,
		"commit (initial): first":                               true,
		"commit (amend): reword":                                true,
		"commit (merge): Merge branch 'topic'":                  true,
		"commit (cherry-pick): picked after a conflict":         true,
		"cherry-pick: add the reader":                           true,
		"cherry-pick: fast-forward":                             false,
		"revert: Revert \"add the reader\"":                     true,
		"merge topic: Merge made by the 'ort' strategy.":        true,
		"merge topic: Fast-forward":                             false,
		"pull: Merge made by the 'ort' strategy.":               true,
		"pull origin main: Fast-forward":                        false,
		"pull --rebase origin main (pick): add the reader":      false,
		"pull --rebase origin main (finish): returning to main": false,
		"rebase (pick): add the reader":                         false,
		"rebase (start): checkout main":                         false,
		"rebase (finish): returning to refs/heads/topic":        false,
		"checkout: moving from main to topic":                   false,
		"reset: moving to HEAD~1":                               false,
		"clone: from https://example.test/team/app.git":         false,
		"am: add the reader":                                    false,
		"committed elsewhere":                                   false,
		"":                                                      false,
	} {
		if got := headLogMadeCommit(message); got != want {
			t.Errorf("headLogMadeCommit(%q) = %v, want %v", message, got, want)
		}
	}
}

func TestParseHeadLogReadsBranchMovesAndCommitsMadeHere(t *testing.T) {
	zero, one, two, three := strings.Repeat("0", 40), strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40)
	line := func(from, to string, at int, message string) string {
		return fmt.Sprintf("%s %s Test Person With Spaces <dev@example.test> %d +0900\t%s\n", from, to, at, message)
	}
	log := line(zero, one, 100, "commit (initial): first") +
		line(one, one, 200, "checkout: moving from main to topic") +
		line(one, two, 300, "commit: second") +
		"not a head log line\n" +
		line(two, one, 400, "checkout: moving from topic to 1111111") +
		line(one, three, 500, "cherry-pick: second") +
		line(three, three, 600, "rebase (finish): returning to refs/heads/topic") +
		line(three, three, 700, "rebase finished: returning to refs/heads/older-git") +
		// A last line without a newline is still a complete record for Git.
		strings.TrimSuffix(line(three, two, 800, "checkout: moving from older-git to main"), "\n")
	before, moves, made, err := parseHeadLog(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	if before != "main" {
		t.Fatalf("before = %q", before)
	}
	wantMoves := []domain.HeadMove{{AtSec: 200, Branch: "topic"}, {AtSec: 400, Branch: ""}, {AtSec: 600, Branch: "topic"}, {AtSec: 700, Branch: "older-git"}, {AtSec: 800, Branch: "main"}}
	if !reflect.DeepEqual(moves, wantMoves) {
		t.Fatalf("moves = %+v", moves)
	}
	wantMade := []domain.MadeCommit{{AtSec: 100, SHA: one}, {AtSec: 300, SHA: two}, {AtSec: 500, SHA: three}}
	if !reflect.DeepEqual(made, wantMade) {
		t.Fatalf("made = %+v", made)
	}
}

func TestParseHeadLogStartingDetached(t *testing.T) {
	sha := strings.Repeat("a", 40)
	before, moves, _, err := parseHeadLog(strings.NewReader(sha + " " + sha + " Dev <dev@example.test> 100 +0000\tcheckout: moving from " + sha + " to topic\n"))
	if err != nil || before != "" || len(moves) != 1 || moves[0].Branch != "topic" {
		t.Fatalf("before=%q moves=%+v err=%v", before, moves, err)
	}
}

func TestParseWorktreeList(t *testing.T) {
	listed := "worktree /work/app\nHEAD " + strings.Repeat("1", 40) + "\nbranch refs/heads/main\n\n" +
		"worktree /work/app-topic\nHEAD " + strings.Repeat("2", 40) + "\nbranch refs/heads/feature/topic\n\n" +
		"worktree /work/app-detached\nHEAD " + strings.Repeat("3", 40) + "\ndetached\n\n" +
		"worktree /work/app-gone\nHEAD " + strings.Repeat("4", 40) + "\nbranch refs/heads/gone\nprunable gitdir file points to non-existent location\n"
	want := []domain.RecordedWorktree{
		{Root: "/work/app", Main: true, Current: "main"},
		{Root: "/work/app-topic", Current: "feature/topic"},
		{Root: "/work/app-detached"},
		{Root: "/work/app-gone", Current: "gone"},
	}
	if got := parseWorktreeList(listed); !reflect.DeepEqual(got, want) {
		t.Fatalf("worktrees = %+v", got)
	}
	bare := "worktree /srv/app.git\nbare\n\nworktree /work/app\nHEAD " + strings.Repeat("1", 40) + "\nbranch refs/heads/main\n"
	if got := parseWorktreeList(bare); len(got) != 1 || got[0].Root != "/work/app" || got[0].Main {
		t.Fatalf("bare repository worktrees = %+v", got)
	}
}

func sessionGitOutput(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func sessionGitCommit(t *testing.T, cwd, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cwd, name+".txt"), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, cwd, "add", name+".txt")
	gitTestRun(t, cwd, "commit", "-m", name)
	return sessionGitOutput(t, cwd, "rev-parse", "HEAD")
}

// sessionGitSnapshot records every path under root with its size and
// modification time, to prove that reading left the repository untouched.
func sessionGitSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		snapshot[path] = fmt.Sprintf("%v %d %d", info.Mode(), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestReadGitRecordsFromARepositoryWithLinkedWorktrees(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	primary, feature, detached := filepath.Join(base, "app"), filepath.Join(base, "app-feature"), filepath.Join(base, "app-detached")
	hooks := filepath.Join(base, "hooks")
	for _, dir := range []string{primary, hooks} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitTestRun(t, primary, "init", "-b", "main")
	for _, setting := range [][2]string{{"user.name", "cxt test"}, {"user.email", "cxt@example.test"}, {"core.hooksPath", hooks}, {"gc.auto", "0"}, {"commit.gpgsign", "false"}} {
		gitTestRun(t, primary, "config", setting[0], setting[1])
	}
	gitTestRun(t, primary, "remote", "add", "origin", "git@example.test:Team/App.git")
	first := sessionGitCommit(t, primary, "first")
	gitTestRun(t, primary, "worktree", "add", "-b", "feature", feature)
	second := sessionGitCommit(t, feature, "second")
	gitTestRun(t, primary, "worktree", "add", "--detach", detached)
	third := sessionGitCommit(t, detached, "third")
	// Without a commit of its own in between, the cherry-pick would write the
	// very same commit object again.
	local := sessionGitCommit(t, primary, "local")
	gitTestRun(t, primary, "cherry-pick", third)
	picked := sessionGitOutput(t, primary, "rev-parse", "HEAD")
	gitTestRun(t, primary, "checkout", "-b", "topic")
	fourth := sessionGitCommit(t, primary, "fourth")
	gitTestRun(t, primary, "checkout", "main")
	gitTestRun(t, primary, "merge", "--no-ff", "-m", "merge topic", "topic")
	merged := sessionGitOutput(t, primary, "rev-parse", "HEAD")
	gitTestRun(t, primary, "revert", "--no-edit", fourth)
	reverted := sessionGitOutput(t, primary, "rev-parse", "HEAD")
	gitTestRun(t, primary, "reset", "--hard", "HEAD~1")
	// A fast-forward moves HEAD without creating a commit here.
	gitTestRun(t, primary, "checkout", "-b", "behind", first)
	gitTestRun(t, primary, "merge", "--ff-only", "main")
	gitTestRun(t, primary, "checkout", "main")

	before := sessionGitSnapshot(t, base)
	records, unread, err := NewSessionGitRecords().ReadGitRecords(context.Background(), filepath.Join(feature))
	if err != nil {
		t.Fatal(err)
	}
	if after := sessionGitSnapshot(t, base); !reflect.DeepEqual(before, after) {
		t.Fatal("reading Git records changed the repository or its worktrees")
	}
	if len(unread) != 0 {
		t.Fatalf("unread = %+v", unread)
	}
	if records.Root != primary || records.Origin != "example.test/team/app" {
		t.Fatalf("root=%q origin=%q", records.Root, records.Origin)
	}
	byRoot := map[string]domain.RecordedWorktree{}
	for _, worktree := range records.Worktrees {
		byRoot[worktree.Root] = worktree
	}
	if len(byRoot) != 3 || !byRoot[primary].Main || byRoot[primary].Current != "main" || byRoot[feature].Current != "feature" || byRoot[detached].Current != "" {
		t.Fatalf("worktrees = %+v", records.Worktrees)
	}
	madeIn := func(root string) []string {
		var shas []string
		for _, made := range byRoot[root].Made {
			shas = append(shas, made.SHA)
		}
		return shas
	}
	if got, want := madeIn(primary), []string{first, local, picked, fourth, merged, reverted}; !reflect.DeepEqual(got, want) {
		t.Fatalf("commits made in the main worktree = %v, want %v", got, want)
	}
	if got, want := madeIn(feature), []string{second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("commits made in the feature worktree = %v, want %v", got, want)
	}
	if got, want := madeIn(detached), []string{third}; !reflect.DeepEqual(got, want) {
		t.Fatalf("commits made in the detached worktree = %v, want %v", got, want)
	}
	var branches []string
	for _, move := range byRoot[primary].Moves {
		branches = append(branches, move.Branch)
	}
	if byRoot[primary].Before != "main" || !reflect.DeepEqual(branches, []string{"topic", "main", "behind", "main"}) {
		t.Fatalf("main worktree before=%q moves=%v", byRoot[primary].Before, branches)
	}
	if len(byRoot[detached].Moves) != 0 {
		t.Fatalf("detached worktree moves = %+v", byRoot[detached].Moves)
	}

	commits := map[string]domain.RecordedCommit{}
	for _, commit := range records.Commits {
		commits[commit.SHA] = commit
	}
	for name, want := range map[string]struct {
		sha      string
		onBranch bool
	}{"first": {first, true}, "second": {second, true}, "picked": {picked, true}, "merged": {merged, true}, "third (detached only)": {third, false}, "reverted (reset away)": {reverted, false}} {
		commit, known := commits[want.sha]
		if !known || commit.OnBranch != want.onBranch || commit.CreatedSec == 0 {
			t.Fatalf("%s: known=%v commit=%+v", name, known, commit)
		}
	}
	// A cherry-pick keeps the author time, author and subject of its source.
	source, copied := commits[third], commits[picked]
	if third == picked || source.AuthoredSec != copied.AuthoredSec || source.AuthorEmail != copied.AuthorEmail || source.Subject != copied.Subject || source.Subject != "third" {
		t.Fatalf("source=%+v copy=%+v", source, copied)
	}

	again, _, err := NewSessionGitRecords().ReadGitRecords(context.Background(), primary)
	if err != nil || !reflect.DeepEqual(again, records) {
		t.Fatalf("a second read from another worktree differs: %v", err)
	}
}

func TestReadGitRecordsOutsideARepositoryAndWithoutCommits(t *testing.T) {
	if _, _, err := NewSessionGitRecords().ReadGitRecords(context.Background(), t.TempDir()); !errors.Is(err, domain.ErrNotGitRepo) {
		t.Fatalf("outside a repository: %v", err)
	}
	empty := t.TempDir()
	gitTestRun(t, empty, "init", "-b", "main")
	records, unread, err := NewSessionGitRecords().ReadGitRecords(context.Background(), empty)
	if err != nil || len(unread) != 0 || len(records.Commits) != 0 || len(records.Worktrees) != 1 || records.Worktrees[0].Current != "main" || records.Origin != "" {
		t.Fatalf("empty repository: records=%+v unread=%+v err=%v", records, unread, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := NewSessionGitRecords().ReadGitRecords(ctx, empty); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
}
