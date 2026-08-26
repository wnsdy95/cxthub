package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestConsumeBriefingRefusesSymlinkedCxtDirectory(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	briefing := filepath.Join(outside, "briefing.json")
	if err := os.WriteFile(briefing, []byte(`{"at":"2099-01-01T00:00:00Z","text":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".cxt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if text, ok := ConsumeBriefing(repo); ok || text != "" {
		t.Fatalf("symlinked briefing consumed: %q", text)
	}
	if data, err := os.ReadFile(briefing); err != nil || len(data) == 0 {
		t.Fatalf("outside briefing changed: %q, %v", data, err)
	}
}

func TestScopedBriefingRefusesSymlinkedQueueDirectory(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".cxt", "briefings")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("TERM_SESSION_ID", "terminal-secret")
	if err := WriteBriefing(repo, "must stay inside repo"); err == nil {
		t.Fatal("scoped briefing followed a symlinked queue directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("scoped briefing escaped repository: %v", entries)
	}
}

func TestBriefingQueuesMultiplePullsUntilNextPrompt(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteBriefing(repo, "pull A context"); err != nil {
		t.Fatal(err)
	}
	if err := WriteBriefing(repo, "pull B context"); err != nil {
		t.Fatal(err)
	}
	text, ok := ConsumeBriefing(repo)
	if !ok || text != "pull A context\n\npull B context" {
		t.Fatalf("queued briefing = %q, ok=%v", text, ok)
	}
	if text, ok := ConsumeBriefing(repo); ok || text != "" {
		t.Fatalf("briefing was consumed twice: %q", text)
	}
}

func TestBriefingIsScopedToInitiatingTerminal(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TERM_SESSION_ID", "terminal-a")
	relative := briefingRelativePath()
	if relative == legacyBriefingRelativePath || strings.Contains(relative, "terminal-a") {
		t.Fatalf("terminal briefing path is not scoped and opaque: %q", relative)
	}
	if err := WriteBriefing(repo, "terminal A pull context"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TERM_SESSION_ID", "terminal-b")
	if err := WriteBriefing(repo, "terminal B pull context"); err != nil {
		t.Fatal(err)
	}
	text, ok := ConsumeBriefing(repo)
	if !ok || text != "terminal B pull context" {
		t.Fatalf("terminal B briefing = %q, ok=%v", text, ok)
	}
	if text, ok := ConsumeBriefing(repo); ok || text != "" {
		t.Fatalf("terminal B briefing was consumed twice: %q", text)
	}

	t.Setenv("TERM_SESSION_ID", "terminal-a")
	text, ok = ConsumeBriefing(repo)
	if !ok || text != "terminal A pull context" {
		t.Fatalf("terminal A briefing = %q, ok=%v", text, ok)
	}
	if text, ok := ConsumeBriefing(repo); ok || text != "" {
		t.Fatalf("terminal A briefing was consumed twice: %q", text)
	}
}

func TestBriefingFallsBackToLiveWrapperScopeWithoutTerminalID(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_SESSION_ID", "")
	t.Setenv("ITERM_SESSION_ID", "")
	t.Setenv("CXT_WRAPPED", "1")
	t.Setenv("CXT_WRAPPED_AGENT", "codex")
	t.Setenv("CXT_WRAPPER_PID", "10101")
	if err := WriteBriefing(repo, "wrapper A pull context"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CXT_WRAPPER_PID", "20202")
	if text, ok := ConsumeBriefing(repo); ok || text != "" {
		t.Fatalf("wrapper B consumed wrapper A briefing: %q", text)
	}

	t.Setenv("CXT_WRAPPER_PID", "10101")
	text, ok := ConsumeBriefing(repo)
	if !ok || text != "wrapper A pull context" {
		t.Fatalf("wrapper A briefing = %q, ok=%v", text, ok)
	}
}

func TestBriefingWithoutDeliveryOwnerUsesLegacyPath(t *testing.T) {
	t.Setenv("TERM_SESSION_ID", "")
	t.Setenv("ITERM_SESSION_ID", "")
	t.Setenv("CXT_WRAPPED", "")
	t.Setenv("CXT_WRAPPER_PID", "")
	t.Setenv("CXT_WRAPPED_AGENT", "")
	if got := briefingRelativePath(); got != legacyBriefingRelativePath {
		t.Fatalf("unowned briefing path = %q, want %q", got, legacyBriefingRelativePath)
	}
}

func TestBoundBriefingEntriesKeepsValidBoundedUTF8(t *testing.T) {
	got := boundBriefingEntries([]string{strings.Repeat("é", briefingMaxBytes)}, briefingMaxBytes)
	if len(got) != 1 || len(got[0]) > briefingMaxBytes || !utf8.ValidString(got[0]) {
		t.Fatalf("bounded briefing bytes=%d valid=%v", len(got[0]), utf8.ValidString(got[0]))
	}
}
