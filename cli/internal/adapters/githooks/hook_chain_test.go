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
			writeHookTestFile(t, hook, script(name, binary))
			writeHookTestFile(t, hook+".pre-cxt", "#!/bin/sh\ncat >/dev/null\nexit 19\n")
			cmd := exec.Command(hook, "prepared")
			cmd.Stdin = strings.NewReader(strings.Repeat("0", 40) + " " + strings.Repeat("a", 40) + " refs/heads/topic\n")
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 19 {
				t.Fatalf("user hook status lost: err=%v output=%s", err, out)
			}
			_, markerErr := os.Stat(marker)
			if strings.HasPrefix(name, "post-") {
				if markerErr != nil {
					t.Fatalf("already completed Git operation was not captured: %v", markerErr)
				}
			} else if !os.IsNotExist(markerErr) {
				t.Fatalf("cxt ran after the user hook rejected: %v", markerErr)
			}
		})
	}
}

func TestManagedReferenceHookObservesCompletedTransactionsAfterUserFailure(t *testing.T) {
	for _, phase := range []string{"committed", "aborted"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "observed")
			binary := filepath.Join(dir, "cxt")
			writeHookTestFile(t, binary, "#!/bin/sh\ncat > "+shellQuote(marker)+"\n")
			hook := filepath.Join(dir, "reference-transaction")
			writeHookTestFile(t, hook, script("reference-transaction", binary))
			writeHookTestFile(t, hook+".pre-cxt", "#!/bin/sh\ncat >/dev/null\nexit 19\n")
			input := strings.Repeat("a", 40) + " " + strings.Repeat("0", 40) + " refs/heads/topic\n"
			cmd := exec.Command(hook, phase)
			cmd.Stdin = strings.NewReader(input)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 19 {
				t.Fatalf("user status lost: %v %s", err, out)
			}
			got, err := os.ReadFile(marker)
			if err != nil || string(got) != input {
				t.Fatalf("transaction observation lost: %v %q", err, got)
			}
		})
	}
}

func TestManagedReferenceHookPreservesGitCreationRejection(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = cleanHookTestEnv()
		return cmd.CombinedOutput()
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-qm", "base"},
	} {
		if out, err := run(args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	hook := filepath.Join(repo, ".git", "hooks", "reference-transaction")
	writeHookTestFile(t, hook, script("reference-transaction", "/usr/bin/true"))
	writeHookTestFile(t, hook+".pre-cxt", "#!/bin/sh\ncat >/dev/null\n[ \"$1\" != prepared ] || exit 19\n")
	if out, err := run("branch", "rejected"); err == nil {
		t.Fatalf("managed hook allowed rejected Git branch: %s", out)
	}
	if _, err := run("show-ref", "--verify", "refs/heads/rejected"); err == nil {
		t.Fatal("rejected branch exists")
	}
}

func TestManagedHookKeepsCxtFailureSeparate(t *testing.T) {
	dir := t.TempDir()
	hook := filepath.Join(dir, "pre-push")
	writeHookTestFile(t, hook, script("pre-push", "/usr/bin/false"))
	writeHookTestFile(t, hook+".pre-cxt", "#!/bin/sh\nexit 0\n")
	if out, err := exec.Command(hook).CombinedOutput(); err != nil {
		t.Fatalf("cxt failure changed the successful user hook status: %v %s", err, out)
	}
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
