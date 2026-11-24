package githooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureGitignoreRefusesSymlinkTarget(t *testing.T) {
	repo := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".gitignore")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := EnsureGitignore(repo); err == nil {
		t.Fatal("symlinked .gitignore was accepted")
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "keep" {
		t.Fatalf("outside target changed: %q err=%v", got, err)
	}
}

func TestHookScriptIsValidShellThatOnlyWakes(t *testing.T) {
	got := script("/usr/local/bin/cxt")
	check := exec.Command("/bin/sh", "-n")
	check.Stdin = strings.NewReader(got)
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("invalid hook shell: %v\n%s", err, out)
	}
	// The hook says which hook it is by its own file name, not by anything
	// Git passed to it.
	if !strings.Contains(got, version+" ") || !strings.Contains(got, `git-hook wake "${0##*/}" "$PPID" </dev/null`) {
		t.Fatalf("the hook does not carry this version's mark or does not wake:\n%s", got)
	}
	// Nothing in the script reads what Git passes to the hook, and nothing
	// in it can end with a status of cxt's.
	for _, forbidden := range []string{"$(cat", "exit $?", "cxt_status"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("the hook script contains %q:\n%s", forbidden, got)
		}
	}
}

func TestInstallRefusesSymlinkedHookTarget(t *testing.T) {
	standIn(t)
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v (%s)", err, out)
	}
	dir, err := hooksDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-hook")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\necho keep\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, HookNames[0])); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if _, err := Install(repo); err == nil {
		t.Fatal("symlinked hook target was accepted")
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "#!/bin/sh\necho keep\n" {
		t.Fatalf("outside hook changed: %q err=%v", got, err)
	}
}

func TestInstallRegistersCxtIgnoreRules(t *testing.T) {
	standIn(t)
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v (%s)", err, out)
	}
	if _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	gitignore, err := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil || !strings.Contains(string(gitignore), ".cxt/") || !strings.Contains(string(gitignore), ".cxtsecrets") {
		t.Fatalf("tracked ignore rules = %q, %v", gitignore, err)
	}
	excludePath, err := exec.Command("git", "-C", repo, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude").Output()
	if err != nil {
		t.Fatal(err)
	}
	exclude, err := os.ReadFile(strings.TrimSpace(string(excludePath)))
	if err != nil || !strings.Contains(string(exclude), ".cxt/") || !strings.Contains(string(exclude), ".cxtsecrets") {
		t.Fatalf("local exclude rules = %q, %v", exclude, err)
	}
}

func TestInstallFallsBackToLocalExcludeForSymlinkedGitignore(t *testing.T) {
	standIn(t)
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v (%s)", err, out)
	}
	outside := filepath.Join(t.TempDir(), "outside-ignore")
	if err := os.WriteFile(outside, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".gitignore")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Install(repo); err != nil {
		t.Fatalf("safe local exclude fallback failed: %v", err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "keep\n" {
		t.Fatalf("symlink target changed: %q, %v", got, err)
	}
	cmd := exec.Command("git", "-C", repo, "check-ignore", ".cxt/probe")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), ".cxt/probe") {
		t.Fatalf("local exclude did not protect .cxt: %v (%s)", err, out)
	}
}

func TestHookScriptQuotesExecutablePathForShell(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "injected")
	maliciousPath := filepath.Join(dir, "$(touch "+marker+")")
	hook := filepath.Join(dir, "hook")
	if err := os.WriteFile(hook, []byte(script(maliciousPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", hook)
	cmd.Env = []string{"PATH="}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook execution failed: %v (%s)", err, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command substitution ran from executable path: %v", err)
	}
}
