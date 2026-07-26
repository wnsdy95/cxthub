package capture

import (
	"os"
	"path/filepath"
	"testing"
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
