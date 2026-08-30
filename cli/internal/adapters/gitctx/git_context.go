package gitctx

import (
	"context"
	"os/exec"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// GitContextAdapter implements the GitContext outbound port using the git CLI.
//
// CurrentRepo: normalizes the remote origin URL of cwd to Repo.ID. falls back to the working tree root (or cwd) if no remote.
// CurrentBranch: git rev-parse --abbrev-ref HEAD. empty if not in a repo.
//
// Safely falls back in non-git or non-repo environments (errors instead of cwd-based identification).
type GitContextAdapter struct{}

// NewGitContextAdapter creates a GitContextAdapter.
func NewGitContextAdapter() *GitContextAdapter {
	return &GitContextAdapter{}
}

// git runs a git subcommand in cwd and returns the trimmed stdout. ("", err) on failure.
func git(ctx context.Context, cwd string, args ...string) (string, error) {
	full := append([]string{"-C", cwd}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CurrentRepo identifies the code repo containing cwd and returns a Repo.
// The normalized remote URL's ContentHash becomes Repo.ID. falls back to the working tree root/cwd if no remote.
// git is the source of truth: always reads from the local .git (working tree), and fails outside a git repo (ErrNotGitRepo — no path fallback).
func (a *GitContextAdapter) CurrentRepo(ctx context.Context, cwd string) (domain.Repo, error) {
	roots, err := ResolveRepositoryRoots(ctx, cwd)
	if err != nil {
		return domain.Repo{}, err
	}
	top := roots.WorktreeRoot

	repo := domain.Repo{LocalPath: roots.SharedRoot, DefaultBranch: a.defaultBranch(ctx, top)}
	if raw, err := git(ctx, top, "config", "--get", "remote.origin.url"); err == nil && raw != "" {
		safeRemote := SanitizeRemoteURL(raw)
		repo.RemoteURL = safeRemote
		repo.GitRemoteURL = safeRemote // credential-free code repo origin — for web "connected" link
		identity := NormalizeRemoteURL(raw)
		if identity == "" {
			identity = top
		}
		repo.ID = domain.HashContent([]byte(identity))
	} else {
		// origin-less git repo: working tree root path as a local-only identifier key (share via cxt remote add).
		repo.ID = domain.HashContent([]byte(roots.SharedRoot))
	}
	return repo, nil
}

// defaultBranch reads the actual default branch from the .git (not the checked-out branch):
// origin/HEAD (recorded during clone) → init.defaultBranch setting → current branch → "main".
func (a *GitContextAdapter) defaultBranch(ctx context.Context, top string) string {
	if ref, err := git(ctx, top, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
		return strings.TrimPrefix(ref, "origin/")
	}
	if b, err := git(ctx, top, "config", "--get", "init.defaultbranch"); err == nil && b != "" {
		return b
	}
	if b, err := git(ctx, top, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && b != "" && b != "HEAD" {
		return b
	}
	return "main"
}

// CurrentBranch returns the current Git branch name for cwd, or an empty string outside a repository.
func (a *GitContextAdapter) CurrentBranch(ctx context.Context, cwd string) (string, error) {
	b, err := git(ctx, cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", nil // Not a repo: empty value (no error; caller should fallback)
	}
	if b == "HEAD" {
		return "", nil // detached
	}
	return b, nil
}

// LocalBranches returns authoritative local refs/heads names, one per line.
func (a *GitContextAdapter) LocalBranches(ctx context.Context, cwd string) ([]string, error) {
	out, err := git(ctx, cwd, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	lines := strings.Split(out, "\n")
	branches := make([]string, 0, len(lines))
	for _, branch := range lines {
		branch = strings.TrimSpace(branch)
		if branch != "" {
			branches = append(branches, branch)
		}
	}
	return branches, nil
}

// Ensure GitContextAdapter implements outbound.GitContext.
var _ outbound.GitContext = (*GitContextAdapter)(nil)
var _ outbound.GitBranchInventory = (*GitContextAdapter)(nil)
