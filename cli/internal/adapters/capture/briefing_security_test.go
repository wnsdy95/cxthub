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

func TestBoundBriefingEntriesKeepsValidBoundedUTF8(t *testing.T) {
	got := boundBriefingEntries([]string{strings.Repeat("é", briefingMaxBytes)}, briefingMaxBytes)
	if len(got) != 1 || len(got[0]) > briefingMaxBytes || !utf8.ValidString(got[0]) {
		t.Fatalf("bounded briefing bytes=%d valid=%v", len(got[0]), utf8.ValidString(got[0]))
	}
}
