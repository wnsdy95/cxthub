package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestCaptureAdmissionPrecedesLiveSessionEffects(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
			t.Run(string(provider)+"/"+event, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				cwd := t.TempDir()
				initHookContext(t, cwd)
				path := writeHookSession(t, cwd, provider, id)
				saver := &recSave{}
				var output bytes.Buffer
				h := NewHandler(capture.NewCaptureCoordinator(saver, domain.TeamIdentity{}))
				called := false
				h.WithCaptureAdmission(func(_ context.Context, got string) error {
					called = true
					if got != cwd {
						t.Fatalf("admitted wrong worktree: %s", got)
					}
					return domain.ErrSelectionChanged
				})
				payload, _ := json.Marshal(hookPayload{SessionID: id, TranscriptPath: path, Cwd: cwd, Prompt: "task"})
				h.stdin, h.stdout = bytes.NewReader(payload), &output
				if err := h.Run(provider, event); !errors.Is(err, domain.ErrSelectionChanged) {
					t.Fatal(err)
				}
				if !called || len(saver.calls) != 0 || output.Len() != 0 || len(capture.ActiveAppSessions(cwd)) != 0 {
					t.Fatal("rejected admission allowed capture, liveness or briefing effects")
				}
				if _, err := os.Stat(filepath.Join(cwd, ".cxt", "capture")); !os.IsNotExist(err) {
					t.Fatalf("capture sidecar appeared: %v", err)
				}
			})
		}
	}
}
