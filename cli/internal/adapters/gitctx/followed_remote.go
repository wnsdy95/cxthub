package gitctx

import (
	"context"
	"fmt"
	"strings"
)

// The Git remote cxt follows. What cxt reads of a repository's Git remote is
// read of one remote: its address names the repository, and its branches say
// what was pushed. That remote is the one named origin unless `cxt setup` was
// told another name, which is kept in the repository's own Git configuration
// and so is the same in every worktree of the clone.

const (
	// FollowedRemoteSetting is the key of that name in Git's configuration.
	FollowedRemoteSetting = "cxt.gitremote"
	defaultFollowedRemote = "origin"
)

// ValidRemoteName reports whether name can stand in a ref name and in a
// configuration key as it is: no space, no pattern character, no empty part.
func ValidRemoteName(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.Contains(name, "//") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`*?[\~^:`, r) {
			return false
		}
	}
	return true
}

// FollowedRemote returns the name of the Git remote cxt follows in the
// repository holding cwd: the name set there, and origin when none is set or
// what is set is no remote name.
func FollowedRemote(ctx context.Context, cwd string) string {
	raw, err := gitRead(ctx, cwd, nil, "config", "--get", FollowedRemoteSetting)
	if name := strings.TrimSpace(string(raw)); err == nil && ValidRemoteName(name) {
		return name
	}
	return defaultFollowedRemote
}

// followedBranches returns where Git keeps the branches of the followed
// remote, without and with the slash that ends the prefix.
func followedBranches(ctx context.Context, cwd string) (string, string) {
	refs := "refs/remotes/" + FollowedRemote(ctx, cwd)
	return refs, refs + "/"
}

// GitRemotes lists the names of the repository's Git remotes.
func GitRemotes(ctx context.Context, cwd string) ([]string, error) {
	out, err := gitRead(ctx, cwd, nil, "remote")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// FollowRemote makes cxt follow the named Git remote in the repository
// holding cwd. The repository must have a remote of that name.
func FollowRemote(ctx context.Context, cwd, name string) error {
	remotes, err := GitRemotes(ctx, cwd)
	if err != nil {
		return err
	}
	for _, remote := range remotes {
		if remote == name {
			_, err := git(ctx, cwd, "config", FollowedRemoteSetting, name)
			return err
		}
	}
	return fmt.Errorf("this repository has no Git remote named %q that cxt can follow (its remotes: %s)", name, strings.Join(remotes, ", "))
}

// FollowOnlyRemote is what `cxt setup` does when it is told no name: where
// nothing is set, the repository has no remote named origin and has exactly
// one remote, that one is followed. It returns the name cxt follows
// afterwards and whether the repository has a remote of that name.
func FollowOnlyRemote(ctx context.Context, cwd string) (string, bool) {
	remotes, err := GitRemotes(ctx, cwd)
	if err != nil {
		return FollowedRemote(ctx, cwd), false
	}
	has := func(name string) bool {
		for _, remote := range remotes {
			if remote == name {
				return true
			}
		}
		return false
	}
	if _, err := gitRead(ctx, cwd, nil, "config", "--get", FollowedRemoteSetting); err != nil && !has(defaultFollowedRemote) && len(remotes) == 1 {
		_ = FollowRemote(ctx, cwd, remotes[0])
	}
	followed := FollowedRemote(ctx, cwd)
	return followed, has(followed)
}
