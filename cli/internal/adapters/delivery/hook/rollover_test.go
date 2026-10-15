package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestLateRolloverHookCannotMutateCurrentSession(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		t.Run(event, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			cwd := t.TempDir()
			initHookContext(t, cwd)
			old := writeHookSession(t, cwd, domain.ProviderCodex, id)
			current := strings.TrimSuffix(old, ".jsonl") + "_22222222-2222-4222-8222-222222222222.jsonl"
			for path, timestamp := range map[string]string{old: "2026-07-31T00:00:00Z", current: "2026-10-09T00:00:00Z"} {
				header, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": id, "cwd": cwd, "timestamp": timestamp}})
				if err := os.WriteFile(path, append(header, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := capture.TrackAppSession(cwd, domain.ProviderCodex, id, current); err != nil {
				t.Fatal(err)
			}
			if err := capture.WriteSessionHandoff(cwd, []string{id}, "current memory"); err != nil {
				t.Fatal(err)
			}
			saver := &recSave{}
			var output bytes.Buffer
			handler := NewHandler(capture.NewCaptureCoordinator(saver, domain.TeamIdentity{}))
			payload, _ := json.Marshal(hookPayload{SessionID: id, TranscriptPath: old, Cwd: cwd, Prompt: "stale"})
			handler.stdin, handler.stdout = bytes.NewReader(payload), &output
			if err := handler.Run(domain.ProviderCodex, event); !errors.Is(err, capture.ErrSessionIdentityMismatch) {
				t.Fatalf("late hook accepted: %v", err)
			}
			if len(saver.calls) != 0 || output.Len() != 0 {
				t.Fatal("late hook captured or emitted briefing")
			}
			if _, err := os.Stat(filepath.Join(cwd, ".cxt", "capture")); !os.IsNotExist(err) {
				t.Fatalf("late hook mutated capture state: %v", err)
			}
			if sessions := capture.ActiveAppSessions(cwd); len(sessions) != 1 || sessions[0].Path != current {
				t.Fatalf("current registry replaced: %+v", sessions)
			}
			if message, ok := capture.ConsumeSessionHandoff(cwd, id); !ok || message != "current memory" {
				t.Fatal("current handoff consumed")
			}
		})
	}
}
