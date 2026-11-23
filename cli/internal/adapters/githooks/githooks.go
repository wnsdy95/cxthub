// Package githooks installs the Git hooks of cxt.
//
// Every hook runs `cxt git-hook wake`: it wakes the reading of the agent
// tools' own session records and of Git. A hook decides nothing and carries
// no data; what was committed, checked out or pushed is read from Git itself.
//
// Design principles:
//   - a hook of cxt cannot stop a Git command; a pre-existing user hook's result is preserved.
//   - preserve existing hooks: user hooks are moved to <name>.pre-cxt and run first (chaining).
//   - idempotent: reinstalling is safe, overwriting with a marker for ownership verification.
package githooks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
)

// Marker indicates that the hook is managed by cxt. Every version's marker begins with it.
const Marker = "# cxt-hook v"

// version is the marker of the scripts this version installs. A script with
// another version's marker is replaced the next time cxt runs in the repository.
const version = Marker + "2"

// HookNames lists the Git hooks installed by cxt. Each one only wakes the
// reading of the agent tools' own session records; none of them decides
// anything, and none of them can stop a Git command.
var HookNames = []string{"post-commit", "post-checkout", "post-merge", "post-rewrite", "pre-push"}

// executable names the program the hooks run. A test puts a stand-in here:
// the program a test runs in is the test itself, and a hook must never start it.
var executable = os.Executable

// earlierHookNames are hooks an earlier version installed and this one does not.
var earlierHookNames = []string{"reference-transaction"}

// hooksDir returns the git hooks directory of repoRoot (worktree/custom hooksPath handling).
func hooksDir(repoRoot string) (string, error) {
	out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "--git-path", "hooks").Output()
	if err != nil {
		return "", fmt.Errorf("not a git repo (%s): initialize git first to install hooks", repoRoot)
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoRoot, dir)
	}
	return dir, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// script generates the hook shell script. It embeds the absolute path to the cxt binary but
// falls back to PATH's cxt if not available (hooks run in restricted environments).
// A hook the user already had runs first, with the hook's own input, and its
// result is the result: cxt never changes whether Git goes on.
func script(cxtBin string) string {
	return fmt.Sprintf(`#!/bin/sh
%s (managed by cxt — 'cxt hooks uninstall' to remove)
user_status=0
if [ -x "$0.pre-cxt" ]; then
  "$0.pre-cxt" "$@" || user_status=$?
fi
CXT=%s
[ -x "$CXT" ] || CXT=cxt
command -v "$CXT" >/dev/null 2>&1 || exit "$user_status"
"$CXT" git-hook wake </dev/null >/dev/null 2>&1 || true
exit "$user_status"
`, version, shellQuote(cxtBin))
}

// Install installs the Git hooks and returns a list of installed hook names.
// Existing user hooks are preserved as <name>.pre-cxt for chaining. Hooks of
// an earlier version that this one does not install are removed.
func Install(repoRoot string) ([]string, error) {
	dir, err := hooksDir(repoRoot)
	if err != nil {
		return nil, err
	}
	if err := EnsureIgnored(repoRoot); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	cxtBin, err := executable()
	if err != nil {
		cxtBin = "cxt"
	}
	var installed []string
	for _, name := range HookNames {
		path := filepath.Join(dir, name)
		if b, err := providerfs.ReadRegularFile(path); err == nil && !strings.Contains(string(b), Marker) {
			// User hooks → moved to chaining target (preserving .pre-cxt if it exists).
			if _, err := os.Stat(path + ".pre-cxt"); os.IsNotExist(err) {
				if err := os.Rename(path, path+".pre-cxt"); err != nil {
					return installed, err
				}
			}
		}
		if err := providerfs.WriteRegularFileAtomic(path, []byte(script(cxtBin)), 0o755); err != nil {
			return installed, err
		}
		installed = append(installed, name)
	}
	if err := remove(dir, earlierHookNames); err != nil {
		return installed, err
	}
	return installed, nil
}

// Upgrade replaces hook scripts an earlier version installed. It does nothing
// in a repository where cxt installed no hook, and nothing when every script
// is already this version's.
func Upgrade(repoRoot string) error {
	dir, err := hooksDir(repoRoot)
	if err != nil {
		return err
	}
	for _, name := range append(append([]string{}, HookNames...), earlierHookNames...) {
		b, err := providerfs.ReadRegularFile(filepath.Join(dir, name))
		if err == nil && strings.Contains(string(b), Marker) && !strings.Contains(string(b), version+" ") {
			_, err := Install(repoRoot)
			return err
		}
	}
	return nil
}

// Uninstall removes cxt hooks and restores preserved user hooks (.pre-cxt).
func Uninstall(repoRoot string) error {
	dir, err := hooksDir(repoRoot)
	if err != nil {
		return err
	}
	return remove(dir, append(append([]string{}, HookNames...), earlierHookNames...))
}

// remove deletes the named hooks that cxt manages and puts back what the user had.
func remove(dir string, names []string) error {
	for _, name := range names {
		path := filepath.Join(dir, name)
		b, err := providerfs.ReadRegularFile(path)
		if err != nil || !strings.Contains(string(b), Marker) {
			continue // if not our hook, do not touch
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if _, err := os.Stat(path + ".pre-cxt"); err == nil {
			if err := os.Rename(path+".pre-cxt", path); err != nil {
				return err
			}
		}
	}
	return nil
}

// cxtIgnoreEntries must never be committed to git. It includes all local files/folders.
// Both .gitignore (team shared) and .git/info/exclude (local) should have the same list.
var cxtIgnoreEntries = []string{
	".cxt/",       // local context store (snapshots, objects, rewrite map, backup stack)
	".cxtsecrets", // list of secret values (plaintext) — forbidden in git and cxthub
}

// EnsureIgnored attempts both the repository-shared and local-only ignore
// defenses. One successful defense is sufficient to keep Git clean (a
// read-only or symlinked project .gitignore must not disable otherwise-safe
// hooks). Lifecycle hooks call this for initialized stores and legacy residue.
func EnsureIgnored(repoRoot string) error {
	_, gitignoreErr := EnsureGitignore(repoRoot)
	excludeErr := EnsureExcluded(repoRoot)
	if gitignoreErr != nil && excludeErr != nil {
		return fmt.Errorf("update .gitignore: %v; update git exclude: %w", gitignoreErr, excludeErr)
	}
	return nil
}

// EnsureGitignore appends the cxt commit prohibition list to the bottom of .gitignore.
// Creates the file if it doesn't exist. Skips existing items (idempotent, preserves user content).
// Returns: items added this time (nil if none).
func EnsureGitignore(repoRoot string) ([]string, error) {
	path := filepath.Join(repoRoot, ".gitignore")
	b, _ := providerfs.ReadRegularFile(path) // start with empty content if file does not exist
	existing := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		existing[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, e := range cxtIgnoreEntries {
		if !existing[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	content := string(b)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if content != "" {
		content += "\n"
	}
	content += "# cxt — local only (commit forbidden)\n" + strings.Join(missing, "\n") + "\n"
	if err := providerfs.WriteRepoFileAtomic(repoRoot, ".gitignore", []byte(content), 0o644); err != nil {
		return nil, err
	}
	return missing, nil
}

// EnsureExcluded registers .cxt/ in .git/info/exclude as well (.gitignore local augmentation —
// even if user deletes from .gitignore, this remains). Idempotent.
func EnsureExcluded(repoRoot string) error {
	out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return err
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoRoot, path)
	}
	b, _ := providerfs.ReadRegularFile(path)
	if strings.Contains(string(b), ".cxt/") && strings.Contains(string(b), ".cxtsecrets") {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := string(b)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if !strings.Contains(content, ".cxt/") {
		content += ".cxt/\n"
	}
	if !strings.Contains(content, ".cxtsecrets") {
		content += ".cxtsecrets\n" // List of secret values — never commit
	}
	return providerfs.WriteRegularFileAtomic(path, []byte(content), 0o644)
}
