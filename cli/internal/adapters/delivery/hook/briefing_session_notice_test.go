package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub-cli/internal/app"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestHandlerBriefingWithSessionNotices(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		t.Run(string(provider), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CXT_WRAPPED", "")
			root := t.TempDir()
			initHookContext(t, root)
			ownerSession := "11111111-1111-4111-8111-111111111111"
			otherSession := "22222222-2222-4222-8222-222222222222"
			paths := map[string]string{
				ownerSession: writeHookSession(t, root, provider, ownerSession),
				otherSession: writeHookSession(t, root, provider, otherSession),
			}
			original := map[string][]byte{}
			for session, path := range paths {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				original[session] = data
			}
			store := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40))
			if err := store.PutWorkingPosition(context.Background(), domain.WorkingPosition{RepoID: string(domain.HashContent([]byte("repo"))), Branch: "main", BranchID: "main", GitCommit: strings.Repeat("a", 40)}); err != nil {
				t.Fatal(err)
			}
			position, err := store.GetWorkingPosition(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			selection := domain.SessionNoticeSelection{RepoID: position.RepoID, WorktreeID: position.WorktreeID, BranchID: position.BranchID, Snapshot: domain.HashContent([]byte("selected")), CodeCommit: position.GitCommit, MemoryPinned: true}
			saver := &recSave{}
			handler := NewHandler(capture.NewCaptureCoordinator(saver, domain.TeamIdentity{})).WithSessionNotices(app.NewSessionNoticeService(hookNoticeSelection{selection}, store))
			incoming := domain.HashContent([]byte("incoming pull"))
			t.Setenv("TERM_SESSION_ID", "pull-terminal")
			if err := capture.WritePullBriefing(root, "main", []domain.ContentHash{incoming}); err != nil {
				t.Fatal(err)
			}
			invoke := func(terminal, session string) (string, error) {
				t.Helper()
				t.Setenv("TERM_SESSION_ID", terminal)
				payload, err := json.Marshal(hookPayload{SessionID: session, TranscriptPath: paths[session], Cwd: root, Prompt: "continue"})
				if err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				handler.stdin, handler.stdout = bytes.NewReader(payload), &output
				runErr := handler.Run(provider, "UserPromptSubmit")
				if output.Len() == 0 {
					return "", runErr
				}
				var decoded struct {
					Output struct {
						Event string `json:"hookEventName"`
						Text  string `json:"additionalContext"`
					} `json:"hookSpecificOutput"`
				}
				if err := json.Unmarshal(output.Bytes(), &decoded); err != nil || decoded.Output.Event != "UserPromptSubmit" {
					t.Fatalf("invalid hook JSON: %s: %v", output.String(), err)
				}
				return decoded.Output.Text, runErr
			}
			if text, err := invoke("other-terminal", ""); !errors.Is(err, domain.ErrHashMismatch) || text != "" {
				t.Fatalf("unscoped notice accepted: %q %v", text, err)
			}
			if text, err := invoke("other-terminal", otherSession); err != nil || !strings.Contains(text, string(selection.ID())) || strings.Contains(text, string(incoming)) || strings.Contains(text, "Incoming snapshot IDs:") {
				t.Fatalf("other terminal must receive only its selection notice: %q %v", text, err)
			}
			if text, err := invoke("other-terminal", otherSession); err != nil || text != "" {
				t.Fatalf("other session notice repeated: %q %v", text, err)
			}
			if text, err := invoke("pull-terminal", ownerSession); err != nil || !strings.Contains(text, string(selection.ID())) || !strings.Contains(text, string(incoming)) || !strings.Contains(text, "Incoming snapshot IDs:") {
				t.Fatalf("initiating terminal must receive briefing and selection: %q %v", text, err)
			}
			if text, err := invoke("pull-terminal", ownerSession); err != nil || text != "" {
				t.Fatalf("briefing or selection repeated for the same session: %q %v", text, err)
			}
			if _, present := capture.ConsumeBriefing(root); present {
				t.Fatal("delivered briefing remains queued")
			}
			if len(saver.calls) != 0 {
				t.Fatal("prompt notice committed a capture")
			}
			for session, path := range paths {
				data, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(data, original[session]) {
					t.Fatalf("native session changed: %s %v", session, err)
				}
			}
		})
	}
}
