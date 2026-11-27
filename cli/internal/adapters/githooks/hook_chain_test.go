package githooks

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedHooksPreserveUserHookFailures(t *testing.T) {
	for _, name := range HookNames {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "cxt-ran")
			binary := filepath.Join(dir, "cxt")
			writeHookTestFile(t, binary, "#!/bin/sh\nprintf ran > "+shellQuote(marker)+"\n")
			hook := filepath.Join(dir, name)
			writeHookTestFile(t, hook, script(binary))
			writeHookTestFile(t, hook+".pre-cxt", "#!/bin/sh\ncat >/dev/null\nexit 19\n")
			cmd := exec.Command(hook, "origin")
			cmd.Stdin = strings.NewReader(strings.Repeat("0", 40) + " " + strings.Repeat("a", 40) + " refs/heads/topic\n")
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 19 {
				t.Fatalf("user hook status lost: err=%v output=%s", err, out)
			}
			// Waking decides nothing, so it happens whatever the user's hook said.
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("the collector was not woken: %v", err)
			}
		})
	}
}

func TestManagedHookKeepsCxtFailureSeparate(t *testing.T) {
	for _, name := range HookNames {
		dir := t.TempDir()
		hook := filepath.Join(dir, name)
		writeHookTestFile(t, hook, script("/usr/bin/false"))
		writeHookTestFile(t, hook+".pre-cxt", "#!/bin/sh\nexit 0\n")
		if out, err := exec.Command(hook).CombinedOutput(); err != nil {
			t.Fatalf("%s: cxt failure changed the successful user hook status: %v %s", name, err, out)
		}
		// Without a hook of the user's, and without any cxt to run, the hook still succeeds.
		if err := os.Remove(hook + ".pre-cxt"); err != nil {
			t.Fatal(err)
		}
		writeHookTestFile(t, hook, script(filepath.Join(dir, "missing")))
		cmd := exec.Command(hook)
		cmd.Env = []string{"PATH="}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: a missing cxt failed the hook: %v %s", name, err, out)
		}
	}
}

// A repository set up by an earlier version holds hook scripts that call
// commands this version no longer has, and one of them could stop a branch
// from being created. Upgrading replaces them all.
func TestUpgradeReplacesTheHooksOfAnEarlierVersion(t *testing.T) {
	standIn(t)
	repo := t.TempDir()
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = cleanHookTestEnv()
		return cmd.CombinedOutput()
	}
	if out, err := run("init", "-q", "-b", "main"); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	dir, err := hooksDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Nothing of cxt's is installed: upgrading writes nothing.
	if err := Upgrade(repo); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entriesNamed(entries, HookNames)) != 0 {
		t.Fatalf("upgrade installed hooks where cxt had none: %v", entries)
	}
	earlier := "#!/bin/sh\n" + Marker + "1 (managed by cxt)\nexit 1\n"
	for _, name := range []string{"post-commit", "reference-transaction"} {
		writeHookTestFile(t, filepath.Join(dir, name), earlier)
	}
	writeHookTestFile(t, filepath.Join(dir, "reference-transaction.pre-cxt"), "#!/bin/sh\nexit 0\n")
	writeHookTestFile(t, filepath.Join(dir, "pre-push"), "#!/bin/sh\n# the user's own\nexit 0\n")
	if err := Upgrade(repo); err != nil {
		t.Fatal(err)
	}
	for _, name := range HookNames {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !strings.Contains(string(got), version+" ") {
			t.Fatalf("%s is not this version's hook: %v\n%s", name, err, got)
		}
	}
	// The hook this version does not install is gone and the user's own is back in its place.
	if got, err := os.ReadFile(filepath.Join(dir, "reference-transaction")); err != nil || strings.Contains(string(got), Marker) {
		t.Fatalf("the earlier reference-transaction hook was not replaced by the user's: %v\n%s", err, got)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "pre-push.pre-cxt")); err != nil || !strings.Contains(string(got), "the user's own") {
		t.Fatalf("the user's pre-push hook was not kept for chaining: %v\n%s", err, got)
	}
	// Git is no longer stopped: a commit and a new branch both succeed.
	for _, args := range [][]string{
		{"-c", "commit.gpgsign=false", "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-qm", "base"},
		{"branch", "topic"},
	} {
		if out, err := run(args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	// A second upgrade changes nothing.
	before, _ := os.Stat(filepath.Join(dir, "post-commit"))
	if err := Upgrade(repo); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(filepath.Join(dir, "post-commit")); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an upgrade with nothing to replace rewrote a hook")
	}
	// Uninstalling removes every hook of cxt's and leaves the user's.
	if err := Uninstall(repo); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "pre-push")); err != nil || !strings.Contains(string(got), "the user's own") {
		t.Fatalf("uninstall did not put the user's pre-push hook back: %v\n%s", err, got)
	}
	if _, err := os.Stat(filepath.Join(dir, "post-commit")); !os.IsNotExist(err) {
		t.Fatalf("uninstall left a hook of cxt's: %v", err)
	}
}

// standIn makes the hooks a test installs run a program that does nothing,
// instead of the test program itself.
func standIn(t *testing.T) {
	t.Helper()
	previous := executable
	executable = func() (string, error) { return "/usr/bin/true", nil }
	t.Cleanup(func() { executable = previous })
}

func entriesNamed(entries []os.DirEntry, names []string) []string {
	var found []string
	for _, entry := range entries {
		for _, name := range names {
			if entry.Name() == name {
				found = append(found, name)
			}
		}
	}
	return found
}

func writeHookTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func cleanHookTestEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") && !strings.HasPrefix(entry, "CXT_") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=gc.auto", "GIT_CONFIG_VALUE_0=0", "GIT_CONFIG_KEY_1=maintenance.auto", "GIT_CONFIG_VALUE_1=false")
}

// The scripts of the version before this one did not say which process runs
// them. A repository that holds only those is upgraded like any other.
func TestUpgradeReplacesTheScriptsOfTheVersionBefore(t *testing.T) {
	standIn(t)
	repo := t.TempDir()
	cmd := exec.Command("git", "-C", repo, "init", "-q", "-b", "main")
	cmd.Env = cleanHookTestEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	dir, err := hooksDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	before := "#!/bin/sh\n" + Marker + "3 (managed by cxt — 'cxt hooks uninstall' to remove)\n" +
		"cxt git-hook wake \"${0##*/}\" </dev/null >/dev/null 2>&1 || true\n"
	for _, name := range HookNames {
		writeHookTestFile(t, filepath.Join(dir, name), before)
	}
	if err := Upgrade(repo); err != nil {
		t.Fatal(err)
	}
	for _, name := range HookNames {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !strings.Contains(string(got), version+" ") || !strings.Contains(string(got), `git-hook wake "${0##*/}" "$PPID" </dev/null`) {
			t.Fatalf("%s is still the script of the version before: %v\n%s", name, err, got)
		}
	}
}
