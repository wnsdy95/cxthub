package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSessionHandoffsAreIsolatedAndConsumedOnce(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.MkdirAll(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const (
		first  = "11111111-1111-4111-8111-111111111111"
		second = "22222222-2222-4222-8222-222222222222"
	)
	if err := WriteSessionHandoff(repo, []string{first, second}, "bounded branch memory"); err != nil {
		t.Fatal(err)
	}
	if got, ok := ConsumeSessionHandoff(repo, first); !ok || got != "bounded branch memory" {
		t.Fatalf("first consume = %q, %v", got, ok)
	}
	if _, ok := ConsumeSessionHandoff(repo, first); ok {
		t.Fatal("first session consumed its handoff twice")
	}
	if got, ok := ConsumeSessionHandoff(repo, second); !ok || got != "bounded branch memory" {
		t.Fatalf("second consume = %q, %v", got, ok)
	}
}

func TestSessionHandoffWorktreeFallback(t *testing.T) {
	repo := t.TempDir()
	cmd := exec.Command("git", "-C", repo, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteSessionHandoff(repo, nil, "fallback memory"); err != nil {
		t.Fatal(err)
	}
	if got, ok := ConsumeSessionHandoff(repo, "late-session-id"); !ok || got != "fallback memory" {
		t.Fatalf("fallback consume = %q, %v", got, ok)
	}
}
