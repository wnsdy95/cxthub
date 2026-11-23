package capture

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const rolloverSession = "11111111-1111-4111-8111-111111111111"
const rolloverInstance = "22222222-2222-4222-8222-222222222222"

func writeRollover(t *testing.T, home, cwd, name, timestamp string) string {
	t.Helper()
	path := filepath.Join(home, ".codex", "sessions", "2026", "10", "09", "rollout-"+name+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": rolloverSession, "cwd": cwd, "timestamp": timestamp}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexCurrentGenerationRejectsInvalidFilenameBinding(t *testing.T) {
	for _, name := range []string{
		"new-" + rolloverInstance,
		"new-" + rolloverSession + "_invalid",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			cwd := stagingSourceRepo(t)
			path := writeRollover(t, home, cwd, name, "2026-10-09T23:19:52.995Z")
			if err := NewCodexCapture().ValidateCurrentSession(context.Background(), path); !errors.Is(err, ErrSessionIdentityMismatch) {
				t.Fatalf("invalid filename binding accepted: %v", err)
			}
		})
	}
}
