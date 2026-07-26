package capture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSecretsRejectsSymlink(t *testing.T) {
	repo := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secrets")
	if err := os.WriteFile(outside, []byte("should-not-load\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, SecretsFile)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if got := LoadSecrets(repo); len(got) != 0 {
		t.Fatalf("symlinked secrets loaded: %q", got)
	}
}

func TestGenerateFromEnvRejectsSymlink(t *testing.T) {
	repo := t.TempDir()
	outside := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(outside, []byte("TOKEN=should-not-copy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".env")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if n, created := GenerateFromEnv(repo); created || n != 0 {
		t.Fatalf("symlinked env generated secrets: n=%d created=%v", n, created)
	}
	if _, err := os.Lstat(filepath.Join(repo, SecretsFile)); !os.IsNotExist(err) {
		t.Fatalf("secrets file unexpectedly created: %v", err)
	}
}
