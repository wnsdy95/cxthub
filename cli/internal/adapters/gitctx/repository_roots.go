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

// ContextRoot returns the real directory that owns an initialized .cxt store.
// The primary worktree wins when both a primary and linked worktree contain
// state, preventing a split repository. Outside Git, the exact cwd retains the
// legacy opt-in behavior used by isolated adapters and tests.
func ContextRoot(ctx context.Context, cwd string) (string, bool) {
	candidates := []string{}
	if roots, err := ResolveRepositoryRoots(ctx, cwd); err == nil {
		candidates = append(candidates, roots.SharedRoot)
		if roots.WorktreeRoot != roots.SharedRoot {
			candidates = append(candidates, roots.WorktreeRoot)
		}
	} else {
		candidates = append(candidates, canonicalRoot(cwd))
	}
	for _, root := range candidates {
		info, err := os.Lstat(filepath.Join(root, ".cxt"))
		if err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir() {
			return root, true
		}
	}
	return "", false
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
