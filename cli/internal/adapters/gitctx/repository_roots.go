package gitctx

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// RepositoryRoots separates the checked-out worktree from the shared cxt
// storage root. Linked worktrees have their own --show-toplevel, while their
// --git-common-dir still points at the primary repository. Desktop coding
// agents commonly run sessions in such linked worktrees.
type RepositoryRoots struct {
	WorktreeRoot string
	SharedRoot   string
}

// ResolveRepositoryRoots returns the current worktree and the primary working
// tree that owns repository-wide cxt state. A normal checkout returns the same
// path for both fields.
func ResolveRepositoryRoots(ctx context.Context, cwd string) (RepositoryRoots, error) {
	worktree, err := git(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil || worktree == "" {
		return RepositoryRoots{}, domain.ErrNotGitRepo
	}
	worktree = canonicalRoot(worktree)

	common, err := git(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common == "" {
		// Older Git versions do not support --path-format. The ordinary form is
		// relative to the worktree when it is not already absolute.
		common, err = git(ctx, cwd, "rev-parse", "--git-common-dir")
		if err != nil || common == "" {
			return RepositoryRoots{}, domain.ErrNotGitRepo
		}
		if !filepath.IsAbs(common) {
			common = filepath.Join(worktree, common)
		}
	}
	common = canonicalRoot(common)

	shared := worktree
	if filepath.Base(common) == ".git" {
		shared = filepath.Dir(common)
	}
	return RepositoryRoots{WorktreeRoot: worktree, SharedRoot: canonicalRoot(shared)}, nil
}

// contextRootCandidates returns the primary and linked-worktree locations that
// may own cxt state. The primary worktree is checked first so repository-wide
// state cannot silently split across linked worktrees.
func contextRootCandidates(ctx context.Context, cwd string) ([]string, bool) {
	candidates := []string{}
	if roots, err := ResolveRepositoryRoots(ctx, cwd); err == nil {
		candidates = append(candidates, roots.SharedRoot)
		if roots.WorktreeRoot != roots.SharedRoot {
			candidates = append(candidates, roots.WorktreeRoot)
		}
		return candidates, true
	} else {
		candidates = append(candidates, canonicalRoot(cwd))
	}
	return candidates, false
}

func regularPath(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
}

func contextDirectory(root string) bool {
	info, err := os.Lstat(filepath.Join(root, ".cxt"))
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir()
}

// ContextRootState separates harmless directory residue from an initialized
// context store without resolving Git roots more than once on hook hot paths.
type ContextRootState struct {
	Root          string
	Exists        bool
	Initialized   bool
	GitRepository bool
}

// InspectContextRoot resolves the preferred initialized store first, then a
// directory-only residue location for ignore migration. Outside Git, an
// existing directory retains the isolated-adapter opt-in behavior.
func InspectContextRoot(ctx context.Context, cwd string) ContextRootState {
	candidates, gitRepo := contextRootCandidates(ctx, cwd)
	if gitRepo {
		for _, root := range candidates {
			if contextDirectory(root) && regularPath(filepath.Join(root, ".cxt", "HEAD")) {
				return ContextRootState{Root: root, Exists: true, Initialized: true, GitRepository: true}
			}
		}
	}
	for _, root := range candidates {
		if contextDirectory(root) {
			return ContextRootState{
				Root: root, Exists: true, Initialized: !gitRepo, GitRepository: gitRepo,
			}
		}
	}
	return ContextRootState{GitRepository: gitRepo}
}

// ExistingContextRoot returns a real .cxt directory even when it is only
// residue from an old/global hook and was never initialized. Callers use this
// solely for safe migration tasks such as adding ignore rules; it must not be
// used as permission to capture or mutate context state.
func ExistingContextRoot(ctx context.Context, cwd string) (string, bool) {
	state := InspectContextRoot(ctx, cwd)
	return state.Root, state.Exists
}

// ContextRoot returns the real directory that owns an initialized .cxt store.
// A directory alone is not initialization: global Codex/Claude hooks run in
// every repository, and legacy versions could leave capture sidecars behind.
// cxt init/setup always writes .cxt/HEAD before installing hooks, making HEAD
// the durable opt-in marker. Outside Git, the directory-only behavior remains
// for isolated adapter use; production capture is Git-repository scoped.
func ContextRoot(ctx context.Context, cwd string) (string, bool) {
	state := InspectContextRoot(ctx, cwd)
	if !state.Initialized {
		return "", false
	}
	return state.Root, true
}

func canonicalRoot(path string) string {
	path = strings.TrimSpace(path)
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return filepath.Clean(path)
}
