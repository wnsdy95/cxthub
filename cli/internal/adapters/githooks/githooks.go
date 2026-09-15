// Package githooks connects cxt to git hooks — "using git means cxt follows along".
//
//	post-commit   → cxt git-hook post-commit    (git commit = context snapshot)
//	post-checkout → cxt git-hook post-checkout  (branch switch = context fork/load)
//	post-merge    → cxt git-hook post-merge     (git pull = context pull)
//	pre-push      → cxt git-hook pre-push       (git push = context push)
//
// Design principles:
//   - cxt failures are fail-open; a pre-existing user hook's failure is preserved.
//   - preserve existing hooks: user hooks are moved to <name>.pre-cxt and run first (chaining).
//   - idempotent: reinstalling is safe, overwriting with a marker for ownership verification.
package githooks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

// Marker indicates that the hook is managed by cxt.
const Marker = "# cxt-hook v1"

// HookNames lists the Git hooks installed by cxt.
//   - reference-transaction (Git 2.28+): the single gateway for every ref change. It observes refs/stash
//     for stash detection and refs/heads/* for branch moves outside checkout, including reset --hard.
//   - post-rewrite: receives old→new commit mappings after rebase/amend, remaps snapshot [git <sha>] links
//     through .cxt/rewrites.json, and synchronizes context when rewriting completes.
var HookNames = []string{"post-commit", "post-checkout", "post-merge", "pre-push", "reference-transaction", "post-rewrite"}

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
func script(hookName, cxtBin string) string {
	// reference-transaction fires on every ref change (including commits), so in the shell,
	// it quickly filters relevant refs to minimize cxt calls. stdin contains lines "<old> <new> <ref>".
	// refs/stash → stash-sync, refs/heads/* → ref-sync (stdin re-supplied — handler determines current branch).
	if hookName == "reference-transaction" {
		return fmt.Sprintf(`#!/bin/sh
%s (managed by cxt — remove with 'cxt hooks uninstall')
input=$(cat)
# Existing user hook chaining (stdin refilling).
user_status=0
if [ -x "$0.pre-cxt" ]; then
  printf '%%s\n' "$input" | "$0.pre-cxt" "$@" || user_status=$?
fi
if [ "$1" = "prepared" ] && [ "$user_status" -ne 0 ]; then exit "$user_status"; fi
CXT=%s
[ -x "$CXT" ] || CXT=cxt
if ! command -v "$CXT" >/dev/null 2>&1; then
  common=$(git rev-parse --git-common-dir 2>/dev/null)
  root=$(git rev-parse --show-toplevel 2>/dev/null)
  if [ "$1" = "prepared" ] && { [ -e "$common/cxt/enabled" ] || [ -e "$root/.cxt/HEAD" ]; }; then
    birth=$(printf '%%s\n' "$input" | awk '$1 ~ /^0+$/ && (length($1)==40 || length($1)==64) && $3 ~ /^refs\/heads\// && $2 !~ /^0+$/ { print "yes" }')
    symbolic=$(printf '%%s\n' "$input" | awk '$1 ~ /^0+$/ && $3=="HEAD" && $2 ~ /^ref:refs\/heads\// { print substr($2,5) }')
    if [ -n "$birth" ] || { [ -n "$symbolic" ] && ! git show-ref --verify --quiet "$symbolic"; }; then
      echo "CXTHub stopped branch creation: cxt executable is unavailable; restore the installed CLI and retry." >&2
      exit 1
    fi
  fi
  exit "$user_status"
fi
case "$input" in
*refs/heads/*)
  printf '%%s\n' "$input" | "$CXT" git-hook branch-transaction "$1" "$PPID"
  cxt_status=$?
  if [ "$1" = "prepared" ] && [ "$cxt_status" -ne 0 ]; then exit "$cxt_status"; fi ;;
esac
if [ "$1" = "prepared" ]; then
  case "$input" in
  *' 0000000000000000000000000000000000000000 refs/heads/'*|*' 0000000000000000000000000000000000000000000000000000000000000000 refs/heads/'*)
    printf '%%s\n' "$input" | "$CXT" git-hook ref-prepare || true ;;
  esac
  exit "$user_status"
fi
if [ "$1" = "aborted" ]; then
  case "$input" in
  *' 0000000000000000000000000000000000000000 refs/heads/'*|*' 0000000000000000000000000000000000000000000000000000000000000000 refs/heads/'*)
    printf '%%s\n' "$input" | "$CXT" git-hook ref-abort || true ;;
  esac
  exit "$user_status"
fi
[ "$1" = "committed" ] || exit "$user_status"
case "$input" in
*refs/stash*) "$CXT" git-hook stash-sync || true ;;
esac
case "$input" in
*refs/heads/*) printf '%%s\n' "$input" | "$CXT" git-hook ref-sync "$PPID" || true ;;
esac
exit "$user_status"
`, Marker, shellQuote(cxtBin))
	}
	// post-rewrite receives old→new commit mapping from stdin — refilled on capture and chaining sides of cxt.
	if hookName == "post-rewrite" {
		return fmt.Sprintf(`#!/bin/sh
%s (managed by cxt — 'cxt hooks uninstall' to remove)
input=$(cat)
user_status=0
if [ -x "$0.pre-cxt" ]; then
  printf '%%s\n' "$input" | "$0.pre-cxt" "$@" || user_status=$?
fi
CXT=%s
[ -x "$CXT" ] || CXT=cxt
command -v "$CXT" >/dev/null 2>&1 || exit "$user_status"
printf '%%s\n' "$input" | "$CXT" git-hook post-rewrite "$@" || true
exit "$user_status"
`, Marker, shellQuote(cxtBin))
	}
	return fmt.Sprintf(`#!/bin/sh
%s (managed by cxt — 'cxt hooks uninstall' to remove)
# Existing user hooks are executed first (chaining).
user_status=0
if [ -x "$0.pre-cxt" ]; then
  "$0.pre-cxt" "$@" || user_status=$?
fi
if [ "%s" = "pre-push" ] && [ "$user_status" -ne 0 ]; then exit "$user_status"; fi
CXT=%s
[ -x "$CXT" ] || CXT=cxt
command -v "$CXT" >/dev/null 2>&1 || exit "$user_status"
"$CXT" git-hook %s "$@" || true
exit "$user_status"
`, Marker, hookName, shellQuote(cxtBin), hookName)
}

// Install installs 4 git hooks and returns a list of installed hook names.
// Existing user hooks are preserved as <name>.pre-cxt for chaining.
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
	cxtBin, err := os.Executable()
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
		if err := providerfs.WriteRegularFileAtomic(path, []byte(script(name, cxtBin)), 0o755); err != nil {
			return installed, err
		}
		installed = append(installed, name)
	}
	// Registration survives loss of the working replica. Do not create it for
	// directory-only residue left by an old global hook.
	if _, err := providerfs.ReadRepoFile(repoRoot, ".cxt/HEAD"); err == nil {
		journal, err := branchjournal.Open(context.Background(), repoRoot)
		if err != nil {
			return installed, err
		}
		if err := journal.Enable(); err != nil {
			return installed, err
		}
	}
	return installed, nil
}

// Uninstall removes cxt hooks and restores preserved user hooks (.pre-cxt).
func Uninstall(repoRoot string) error {
	dir, err := hooksDir(repoRoot)
	if err != nil {
		return err
	}
	for _, name := range HookNames {
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
