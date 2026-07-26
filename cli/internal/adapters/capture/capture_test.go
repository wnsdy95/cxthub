package capture

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestEncodeCwd(t *testing.T) {
	if got := encodeCwd("/Users/work/foo"); got != "-Users-work-foo" {
		t.Fatalf("encodeCwd = %q", got)
	}
	if got := encodeCwd("/a.b/c"); got != "-a-b-c" {
		t.Fatalf("encodeCwd dots = %q", got)
	}
}

func TestNewSessionID(t *testing.T) {
	id := newSessionID()
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Fatalf("uuid format: %q", id)
	}
	if id[14] != '4' { // version nibble
		t.Fatalf("not v4: %q", id)
	}
	if newSessionID() == newSessionID() {
		t.Fatal("ids should be unique")
	}
}

func TestClaudeLocateActiveSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/Users/work/proj"
	dir := filepath.Join(home, ".claude", "projects", encodeCwd(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	older := filepath.Join(dir, "old.jsonl")
	newer := filepath.Join(dir, "new.jsonl")
	os.WriteFile(older, []byte(`{"type":"user"}`), 0o644)
	os.WriteFile(newer, []byte(`{"type":"user"}`), 0o644)
	// Adjust mtime: newer is more recent
	os.Chtimes(older, time.Unix(1000, 0), time.Unix(1000, 0))
	os.Chtimes(newer, time.Unix(2000, 0), time.Unix(2000, 0))

	got, err := NewClaudeCapture().LocateActiveSession(context.Background(), cwd)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if filepath.Base(got) != "new.jsonl" {
		t.Fatalf("expected newest (new.jsonl), got %s", got)
	}
}

func TestClaudeLocateNoSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := NewClaudeCapture().LocateActiveSession(context.Background(), "/nonexistent/path")
	if err != domain.ErrNoActiveSession {
		t.Fatalf("expected ErrNoActiveSession, got %v", err)
	}
}

func TestCodexLocateActiveSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/Users/work/myrepo"
	day := filepath.Join(home, ".codex", "sessions", "2026", "06", "30")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	// Rollout for matching cwd
	match := filepath.Join(day, "rollout-2026-06-30T01-00-00-aaaa.jsonl")
	os.WriteFile(match, []byte(`{"timestamp":"t","type":"session_meta","payload":{"cwd":"`+cwd+`"}}`+"\n"), 0o644)
	// Rollout for different cwd (to be ignored)
	other := filepath.Join(day, "rollout-2026-06-30T02-00-00-bbbb.jsonl")
	os.WriteFile(other, []byte(`{"timestamp":"t","type":"session_meta","payload":{"cwd":"/somewhere/else"}}`+"\n"), 0o644)
	os.Chtimes(match, time.Unix(1000, 0), time.Unix(1000, 0))
	os.Chtimes(other, time.Unix(5000, 0), time.Unix(5000, 0)) // More recent but cwd mismatch

	got, err := NewCodexCapture().LocateActiveSession(context.Background(), cwd)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if filepath.Base(got) != filepath.Base(match) {
		t.Fatalf("expected cwd-matching rollout, got %s", got)
	}
}
