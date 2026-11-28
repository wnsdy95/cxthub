package gitctx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func branchIDTestRepo(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := filepath.Join(base, "app")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	branchIDTestGit(t, repo, "init", "-b", "main")
	for _, setting := range [][2]string{{"user.name", "cxt test"}, {"user.email", "cxt@example.test"}, {"commit.gpgsign", "false"}} {
		branchIDTestGit(t, repo, "config", setting[0], setting[1])
	}
	branchIDTestGit(t, repo, "commit", "--allow-empty", "-m", "first")
	return repo
}

func branchIDTestGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Git keeps the setting with the branch: this is what the ID relies on.
func TestBranchIDsFollowWhatGitDoesToTheBranch(t *testing.T) {
	repo := branchIDTestRepo(t)
	ctx := context.Background()
	ids := NewBranchIDs()
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)

	// A repository cxt has not touched: branches, and no IDs.
	branchIDTestGit(t, repo, "branch", "topic/one.two")
	listed, err := ids.LocalBranches(ctx, repo)
	if want := []domain.LocalBranch{{Name: "main"}, {Name: "topic/one.two"}}; err != nil || !reflect.DeepEqual(listed, want) {
		t.Fatalf("no IDs yet: %+v %v", listed, err)
	}
	if err := ids.StoreBranchID(ctx, repo, domain.BranchIDAssignment{Name: "main", ID: a}); err != nil {
		t.Fatal(err)
	}
	if err := ids.StoreBranchID(ctx, repo, domain.BranchIDAssignment{Name: "topic/one.two", ID: b}); err != nil {
		t.Fatal(err)
	}
	listed, err = ids.LocalBranches(ctx, repo)
	want := []domain.LocalBranch{{Name: "main", ID: a, IssuedFor: "main"}, {Name: "topic/one.two", ID: b, IssuedFor: "topic/one.two"}}
	if err != nil || !reflect.DeepEqual(listed, want) {
		t.Fatalf("stored IDs: %+v %v", listed, err)
	}

	// A rename moves the setting; the issued-for name still says where it came from.
	branchIDTestGit(t, repo, "branch", "-m", "topic/one.two", "Topic-Renamed")
	// A copy copies the setting.
	branchIDTestGit(t, repo, "branch", "-c", "main", "main-copy")
	listed, err = ids.LocalBranches(ctx, repo)
	want = []domain.LocalBranch{
		{Name: "Topic-Renamed", ID: b, IssuedFor: "topic/one.two"},
		{Name: "main", ID: a, IssuedFor: "main"},
		{Name: "main-copy", ID: a, IssuedFor: "main"},
	}
	if err != nil || !reflect.DeepEqual(listed, want) {
		t.Fatalf("after a rename and a copy: %+v %v", listed, err)
	}
	// The one rule over what was read: only the copy gets a new ID.
	plan := domain.PlanBranchIDs(listed, func() string { return strings.Repeat("c", 32) })
	if !reflect.DeepEqual(plan, []domain.BranchIDAssignment{{Name: "main-copy", ID: strings.Repeat("c", 32)}}) {
		t.Fatalf("plan: %+v", plan)
	}

	// Deleting the branch drops the setting, and the same name made again starts without an ID.
	branchIDTestGit(t, repo, "branch", "-D", "Topic-Renamed")
	branchIDTestGit(t, repo, "branch", "Topic-Renamed")
	// A linked worktree sees the same settings.
	linked := filepath.Join(filepath.Dir(repo), "linked")
	branchIDTestGit(t, repo, "worktree", "add", "-q", linked, "main-copy")
	listed, err = ids.LocalBranches(ctx, linked)
	want = []domain.LocalBranch{
		{Name: "Topic-Renamed"},
		{Name: "main", ID: a, IssuedFor: "main"},
		{Name: "main-copy", ID: a, IssuedFor: "main"},
	}
	if err != nil || !reflect.DeepEqual(listed, want) {
		t.Fatalf("from a linked worktree, after a delete and a re-create: %+v %v", listed, err)
	}
	// An ID stored from the linked worktree is the repository's.
	if err := ids.StoreBranchID(ctx, linked, domain.BranchIDAssignment{Name: "main-copy", ID: b}); err != nil {
		t.Fatal(err)
	}
	if got := branchIDTestGit(t, repo, "config", "--local", "--get", "branch.main-copy.cxtBranchId"); got != b {
		t.Fatalf("the main checkout reads %q", got)
	}
	if got := branchIDTestGit(t, repo, "config", "--local", "--get", "branch.main-copy.cxtBranchIdIssuedFor"); got != "main-copy" {
		t.Fatalf("issued for %q", got)
	}
}

func TestBranchIDsRefuseWhatIsNotAnAssignment(t *testing.T) {
	repo := branchIDTestRepo(t)
	ids := NewBranchIDs()
	for _, assignment := range []domain.BranchIDAssignment{{Name: "", ID: strings.Repeat("a", 32)}, {Name: "main", ID: "short"}, {Name: "main"}} {
		if err := ids.StoreBranchID(context.Background(), repo, assignment); err == nil {
			t.Fatalf("%+v was stored", assignment)
		}
	}
	if listed, err := ids.LocalBranches(context.Background(), repo); err != nil || !reflect.DeepEqual(listed, []domain.LocalBranch{{Name: "main"}}) {
		t.Fatalf("a refused assignment wrote something: %+v %v", listed, err)
	}
	if _, err := ids.LocalBranches(context.Background(), t.TempDir()); err == nil {
		t.Fatal("a folder that is not a repository must fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ids.LocalBranches(ctx, repo); err == nil {
		t.Fatal("a cancelled read must fail")
	}
	first, err := NewBranchID()
	second, err2 := NewBranchID()
	if err != nil || err2 != nil || !domain.ValidBranchID(first) || first == second {
		t.Fatalf("random IDs: %q %q %v %v", first, second, err, err2)
	}
}

// A branch is on origin when Git's own records say so: its configured
// upstream on origin, or else a remote-tracking branch with the same name.
func TestBranchIDsReadTheNameOnOriginFromGitRecords(t *testing.T) {
	repo := branchIDTestRepo(t)
	origin := filepath.Join(filepath.Dir(repo), "origin.git")
	branchIDTestGit(t, filepath.Dir(repo), "init", "--bare", "-b", "main", origin)
	branchIDTestGit(t, repo, "remote", "add", "origin", origin)
	// Another remote never counts.
	other := filepath.Join(filepath.Dir(repo), "other.git")
	branchIDTestGit(t, filepath.Dir(repo), "init", "--bare", "-b", "main", other)
	branchIDTestGit(t, repo, "remote", "add", "other", other)

	branchIDTestGit(t, repo, "push", "-q", "-u", "origin", "main")
	branchIDTestGit(t, repo, "branch", "local-name")
	branchIDTestGit(t, repo, "push", "-q", "origin", "local-name:name-on-origin")
	branchIDTestGit(t, repo, "branch", "--set-upstream-to=origin/name-on-origin", "local-name")
	// Pushed without an upstream: the same name is on origin.
	branchIDTestGit(t, repo, "branch", "same-name")
	branchIDTestGit(t, repo, "push", "-q", "origin", "same-name")
	branchIDTestGit(t, repo, "branch", "never-pushed")
	branchIDTestGit(t, repo, "branch", "on-another-remote")
	branchIDTestGit(t, repo, "push", "-q", "-u", "other", "on-another-remote")
	// Its upstream was deleted on origin and pruned here.
	branchIDTestGit(t, repo, "branch", "pruned")
	branchIDTestGit(t, repo, "push", "-q", "-u", "origin", "pruned")
	branchIDTestGit(t, repo, "push", "-q", "origin", "--delete", "pruned")

	listed, err := NewBranchIDs().LocalBranches(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, branch := range listed {
		got[branch.Name] = branch.Remote
	}
	want := map[string]string{"main": "main", "local-name": "name-on-origin", "same-name": "same-name", "never-pushed": "", "on-another-remote": "", "pruned": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("names on origin: %v", got)
	}
}

func TestBranchIDLockSerializesAssignments(t *testing.T) {
	repo := branchIDTestRepo(t)
	ids := NewBranchIDs()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 2)
	go func() {
		done <- ids.WithBranchIDLock(context.Background(), repo, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	// A second holder waits; with its context cancelled it gives up.
	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	ran := false
	go func() {
		second <- ids.WithBranchIDLock(ctx, repo, func() error { ran = true; return nil })
	}()
	cancel()
	if err := <-second; err == nil || ran {
		t.Fatalf("a second holder ran inside the lock: err=%v ran=%v", err, ran)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Released: the next holder runs, and its error is returned as is.
	failure := os.ErrInvalid
	if err := ids.WithBranchIDLock(context.Background(), repo, func() error { return failure }); err != failure {
		t.Fatalf("error from inside the lock: %v", err)
	}
	// The lock lives beside Git's own files, never in the working tree.
	if status := branchIDTestGit(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("the lock shows in the working tree: %s", status)
	}
	if err := ids.WithBranchIDLock(context.Background(), t.TempDir(), func() error { return nil }); err == nil {
		t.Fatal("a folder that is not a repository must fail")
	}
}
