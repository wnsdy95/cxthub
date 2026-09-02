package gitctx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitTestRun(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestResolveRepositoryRootsSharesPrimaryContextWithLinkedWorktree(t *testing.T) {
	primary := filepath.Join(t.TempDir(), "primary")
	linked := filepath.Join(t.TempDir(), "app-worktree")
	if err := os.MkdirAll(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, primary, "init", "-b", "main")
	gitTestRun(t, primary, "config", "user.name", "cxt test")
	gitTestRun(t, primary, "config", "user.email", "cxt@example.test")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, primary, "config", "core.hooksPath", hooks)
	gitTestRun(t, primary, "config", "gc.auto", "0")
	if err := os.WriteFile(filepath.Join(primary, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, primary, "add", "README.md")
	gitTestRun(t, primary, "commit", "-m", "initial")
	if err := os.Mkdir(filepath.Join(primary, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(primary, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, primary, "worktree", "add", "-b", "app/session", linked)
	primary = canonicalRoot(primary)
	linked = canonicalRoot(linked)

	subdir := filepath.Join(linked, "nested")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	roots, err := ResolveRepositoryRoots(context.Background(), subdir)
	if err != nil {
		t.Fatal(err)
	}
	if roots.WorktreeRoot != linked || roots.SharedRoot != primary {
		t.Fatalf("roots=%+v, want worktree=%q shared=%q", roots, linked, primary)
	}
	if root, ok := ContextRoot(context.Background(), subdir); !ok || root != primary {
		t.Fatalf("context root=%q ok=%v, want primary %q", root, ok, primary)
	}

	repo, err := NewGitContextAdapter().CurrentRepo(context.Background(), subdir)
	if err != nil {
		t.Fatal(err)
	}
	if repo.LocalPath != primary {
		t.Fatalf("repo local path=%q, want shared root %q", repo.LocalPath, primary)
	}
}

func TestContextRootRequiresInitializedHeadInGitRepository(t *testing.T) {
	repo := t.TempDir()
	gitTestRun(t, repo, "init", "-b", "main")
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}

	if root, ok := ExistingContextRoot(context.Background(), repo); !ok || root != canonicalRoot(repo) {
		t.Fatalf("existing root=%q ok=%v, want residue root %q", root, ok, canonicalRoot(repo))
	}
	if root, ok := ContextRoot(context.Background(), repo); ok || root != "" {
		t.Fatalf("directory-only residue enabled capture: root=%q ok=%v", root, ok)
	}

	if err := os.WriteFile(filepath.Join(repo, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if root, ok := ContextRoot(context.Background(), repo); !ok || root != canonicalRoot(repo) {
		t.Fatalf("initialized root=%q ok=%v, want %q", root, ok, canonicalRoot(repo))
	}
}

func TestContextRootRejectsSymlinkedStore(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".cxt")); err != nil {
		t.Fatal(err)
	}
	if got, ok := ContextRoot(context.Background(), root); ok || got != "" {
		t.Fatalf("symlinked store accepted: root=%q ok=%v", got, ok)
	}
}
