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

func TestReferenceTransactionScriptCarriesPreparedStateByBranch(t *testing.T) {
	got := script("reference-transaction", "/usr/local/bin/cxt")
	check := exec.Command("/bin/sh", "-n")
	check.Stdin = strings.NewReader(got)
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("invalid reference-transaction shell: %v\n%s", err, out)
	}
	for _, want := range []string{
		`git-hook ref-prepare`,
		`git-hook ref-abort`,
		`git-hook ref-sync "$PPID"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("reference-transaction hook lacks %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{`ref-prepare "$PPID"`, `ref-abort "$PPID"`} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("prepared lifecycle is incorrectly keyed by unstable hook PPID %q:\n%s", forbidden, got)
		}
	}
}

func TestInstallRefusesSymlinkedHookTarget(t *testing.T) {
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

func TestHookScriptQuotesExecutablePathForShell(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "injected")
	maliciousPath := filepath.Join(dir, "$(touch "+marker+")")
	hook := filepath.Join(dir, "hook")
	if err := os.WriteFile(hook, []byte(script("post-commit", maliciousPath)), 0o755); err != nil {
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
