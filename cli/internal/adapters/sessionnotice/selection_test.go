package sessionnotice

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"os"
	"path/filepath"
	"testing"
)

type positionsFake struct {
	p      domain.WorkingPosition
	onRead func()
}

func (f *positionsFake) GetWorkingPosition(context.Context) (domain.WorkingPosition, error) {
	if f.onRead != nil {
		f.onRead()
	}
	return f.p, nil
}
func runGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), root, append([]string{"-c", "core.hooksPath=/dev/null", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "commit.gpgSign=false", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test"}, args...)...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}
func selectionFixture(t *testing.T) (*SelectionReader, *positionsFake, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "commit", "--allow-empty", "-m", "first")
	gitDir := runGit(t, root, "rev-parse", "--absolute-git-dir")
	head := runGit(t, root, "rev-parse", "HEAD")
	hash := sha256.Sum256([]byte(gitDir))
	if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	origin := "https://cxthub.example/acme/project"
	if err := remotecfg.Save(root, remotecfg.Remotes{"origin": origin}); err != nil {
		t.Fatal(err)
	}
	p := &positionsFake{p: domain.WorkingPosition{RepoID: string(remotecfg.RepoIDFor(origin)), WorktreeID: fmt.Sprintf("%x", hash[:16]), Branch: "main", BranchID: "branch-main", Snapshot: domain.HashContent([]byte("snapshot")), GitCommit: head, MemoryPinned: true}}
	return NewSelectionReader(root, gitDir, p), p, root
}
func TestReadNoticeSelectionIgnoresPendingAndSharedProgress(t *testing.T) {
	src, p, root := selectionFixture(t)
	ctx := context.Background()
	before, err := src.ReadNoticeSelection(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	p.p.SharedTarget = domain.HashContent([]byte("shared advanced"))
	p.p.Selection = &domain.HistoryEvent{ID: "timestamp-only"}
	if err = os.MkdirAll(filepath.Join(root, ".cxt", "pending"), 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, ".cxt", "pending", "another.json"), []byte(`{"updated":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := src.ReadNoticeSelection(ctx, root)
	if err != nil || after != before {
		t.Fatalf("irrelevant change: %+v %v", after, err)
	}
	p.p.MemoryHash = domain.HashContent([]byte("new pinned"))
	after, err = src.ReadNoticeSelection(ctx, root)
	if err != nil || after.ID() == before.ID() || after.MemorySource != p.p.Snapshot {
		t.Fatalf("repin=%+v %v", after, err)
	}
}
func TestReadNoticeSelectionRejectsWrongIdentityAndConcurrentCode(t *testing.T) {
	for _, kind := range []string{"repo", "worktree", "code", "branch", "race", "foreign-cwd"} {
		t.Run(kind, func(t *testing.T) {
			src, p, root := selectionFixture(t)
			cwd := root
			switch kind {
			case "repo":
				p.p.RepoID = string(domain.HashContent([]byte("wrong")))
			case "worktree":
				p.p.WorktreeID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			case "code":
				p.p.GitCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			case "branch":
				p.p.Branch = "other"
			case "race":
				p.onRead = func() { runGit(t, root, "commit", "--allow-empty", "-m", "racing") }
			case "foreign-cwd":
				cwd = t.TempDir()
				runGit(t, cwd, "init", "-b", "main")
				runGit(t, cwd, "commit", "--allow-empty", "-m", "foreign")
			}
			if _, err := src.ReadNoticeSelection(context.Background(), cwd); !errors.Is(err, domain.ErrSelectionChanged) {
				t.Fatalf("accepted %s: %v", kind, err)
			}
		})
	}
}
func TestReadNoticeSelectionUsesExactWorktreeDespiteGitEnvironment(t *testing.T) {
	src, _, root := selectionFixture(t)
	other := t.TempDir()
	runGit(t, other, "init", "-b", "foreign")
	runGit(t, other, "commit", "--allow-empty", "-m", "foreign")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	if _, err := src.ReadNoticeSelection(context.Background(), root); err != nil {
		t.Fatal(err)
	}
}
func TestReadNoticeSelectionMissingAndUnsafeStore(t *testing.T) {
	src, _, root := selectionFixture(t)
	path := filepath.Join(root, ".cxt", "HEAD")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := src.ReadNoticeSelection(context.Background(), root); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "HEAD")
	if err := os.WriteFile(target, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := src.ReadNoticeSelection(context.Background(), root); err == nil {
		t.Fatal("followed HEAD symlink")
	}
}

func TestReadNoticeSelectionLinkedWorktreeHasIndependentCursor(t *testing.T) {
	src, _, root := selectionFixture(t)
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, root, "worktree", "add", "--no-track", "-b", "feature", linked, "HEAD")
	dir := runGit(t, linked, "rev-parse", "--absolute-git-dir")
	sum := sha256.Sum256([]byte(dir))
	p := &positionsFake{p: domain.WorkingPosition{RepoID: string(remotecfg.RepoIDFor("https://cxthub.example/acme/project")), WorktreeID: fmt.Sprintf("%x", sum[:16]), Branch: "feature", BranchID: "feature", Snapshot: domain.HashContent([]byte("feature snapshot")), GitCommit: runGit(t, linked, "rev-parse", "HEAD"), MemoryPinned: true}}
	other := NewSelectionReader(root, dir, p)
	main, err := src.ReadNoticeSelection(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	feature, err := other.ReadNoticeSelection(context.Background(), linked)
	if err != nil {
		t.Fatal(err)
	}
	if feature.WorktreeID == main.WorktreeID || feature.Snapshot == main.Snapshot || feature.CodeCommit != main.CodeCommit {
		t.Fatalf("worktree isolation: main=%+v linked=%+v", main, feature)
	}
	if _, err = src.ReadNoticeSelection(context.Background(), linked); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatalf("primary accepted linked: %v", err)
	}
}
