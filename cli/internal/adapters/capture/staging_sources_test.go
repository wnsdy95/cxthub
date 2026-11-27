package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func stagingSourceRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "-c", "core.hooksPath=/dev/null", "init", "-q", "-b", "main", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}
