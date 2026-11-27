package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func publicationFixtureGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory, "-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgsign=false", "-c", "user.name=Publication Fixture", "-c", "user.email=publication@example.test"}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func newPublicationGitFixture(t *testing.T) (publicationLocalGit, []string) {
	t.Helper()
	directory := t.TempDir()
	git := func(args ...string) string { return publicationFixtureGit(t, directory, args...) }
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(directory, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-b", "main")
	git("remote", "add", "origin", publicationGitOrigin)
	write("nested/file", "base\n")
	write("deleted", "removed later\n")
	write("renamed", "moved later\n")
	write("executable", "executable later\n")
	if err := os.Symlink("nested/file", filepath.Join(directory, "link")); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "root")
	root := git("rev-parse", "HEAD")
	git("switch", "-qc", "feature")
	write("nested/file", "feature\n")
	git("rm", "deleted")
	git("mv", "renamed", "nested/moved")
	if err := os.Chmod(filepath.Join(directory, "executable"), 0700); err != nil {
		t.Fatal(err)
	}
	git("update-index", "--chmod=+x", "executable")
	git("add", "nested/file")
	git("commit", "-qm", "feature")
	feature := git("rev-parse", "HEAD")
	git("switch", "-q", "main")
	write("main-only", "main\n")
	git("add", "main-only")
	git("commit", "-qm", "main")
	main := git("rev-parse", "HEAD")
	git("merge", "--no-ff", "-qm", "merge", "feature")
	return publicationLocalGit{directory: directory}, []string{root, feature, main, git("rev-parse", "HEAD")}
}

func TestPublicationLocalGitBoundsFailureDiagnostics(t *testing.T) {
	reader, _ := newPublicationGitFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := reader.git(ctx, "-c", "alias.fixture-failure=!printf '%03000d' 0 >&2; exit 7", "fixture-failure")
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 7 || !strings.Contains(err.Error(), " [truncated]") || len(err.Error()) > 2300 {
		t.Fatalf("missing bounded Git failure diagnostic: %v", err)
	}
}
