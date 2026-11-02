package capture

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppSessionRootsUsesOneGitResolutionPerCall(test *testing.T) {
	root := stagingSourceRepo(test)
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		test.Fatal(err)
	}
	cwd := filepath.Join(root, "nested")
	if err := os.Mkdir(cwd, 0700); err != nil {
		test.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		test.Fatal(err)
	}
	bin := test.TempDir()
	log := filepath.Join(bin, "calls")
	test.Setenv("CXT_TEST_ROOT_GIT", realGit)
	test.Setenv("CXT_TEST_ROOT_CALLS", log)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CXT_TEST_ROOT_CALLS\"\nexec \"$CXT_TEST_ROOT_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		test.Fatal(err)
	}
	test.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for attempt := 0; attempt < 3; attempt++ {
		store, worktree, enabled := appSessionRoots(context.Background(), cwd)
		if attempt == 1 {
			if enabled || store != "" || worktree != "" {
				test.Fatal("removed initialization was cached", store, worktree, enabled)
			}
		} else if !enabled || store != canonical || worktree != canonical {
			test.Fatal("root selection changed", store, worktree, enabled)
		}
		calls, err := os.ReadFile(log)
		if err != nil {
			test.Fatal(err)
		}
		if count := len(strings.Split(strings.TrimSpace(string(calls)), "\n")); count != 2*(attempt+1) {
			test.Fatalf("Git root observation repeated: got %d calls, want %d", count, 2*(attempt+1))
		}
		if attempt == 0 {
			if err := os.Remove(filepath.Join(root, ".cxt", "HEAD")); err != nil {
				test.Fatal(err)
			}
		} else if attempt == 1 {
			if err := os.WriteFile(filepath.Join(root, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
				test.Fatal(err)
			}
		}
	}
}
