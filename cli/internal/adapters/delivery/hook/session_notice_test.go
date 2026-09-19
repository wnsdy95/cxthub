package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type hookNoticeSelection struct{ selection domain.SessionNoticeSelection }

func (f hookNoticeSelection) ReadNoticeSelection(context.Context, string) (domain.SessionNoticeSelection, error) {
	return f.selection, nil
}

type shortHookWriter struct{}

func (shortHookWriter) Write(b []byte) (int, error) { return len(b) - 1, nil }
func TestHandlerSessionNoticeProviderJSONAndRetry(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderCodex, domain.ProviderClaude} {
		for _, event := range []string{"SessionStart", "UserPromptSubmit"} {
			t.Run(string(provider)+"/"+event, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				root := t.TempDir()
				initHookContext(t, root)
				id := "11111111-1111-4111-8111-111111111111"
				path := writeHookSession(t, root, provider, id)
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				store := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40))
				// Cursor ownership is derived by the same constructor used in production.
				if err = store.PutWorkingPosition(context.Background(), domain.WorkingPosition{RepoID: string(domain.HashContent([]byte("repo"))), Branch: "main", BranchID: "main", GitCommit: strings.Repeat("a", 40)}); err != nil {
					t.Fatal(err)
				}
				p, err := store.GetWorkingPosition(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				selected := domain.SessionNoticeSelection{RepoID: p.RepoID, WorktreeID: p.WorktreeID, BranchID: p.BranchID, Snapshot: domain.HashContent([]byte("selected")), CodeCommit: p.GitCommit, MemoryPinned: true}
				h := NewHandler(capture.NewCaptureCoordinator(&recSave{}, domain.TeamIdentity{})).WithSessionNotices(app.NewSessionNoticeService(hookNoticeSelection{selected}, store))
				payload, _ := json.Marshal(hookPayload{SessionID: id, TranscriptPath: path, Cwd: root, Prompt: "hello"})
				h.stdin = bytes.NewReader(payload)
				h.stdout = shortHookWriter{}
				if err = h.Run(provider, event); !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("short output=%v", err)
				}
				var output bytes.Buffer
				h.stdin = bytes.NewReader(payload)
				h.stdout = &output
				if err = h.Run(provider, event); err != nil {
					t.Fatal(err)
				}
				var decoded struct {
					Output struct {
						Event string `json:"hookEventName"`
						Text  string `json:"additionalContext"`
					} `json:"hookSpecificOutput"`
				}
				if err = json.Unmarshal(output.Bytes(), &decoded); err != nil {
					t.Fatalf("JSON=%q %v", output.String(), err)
				}
				if decoded.Output.Event != event || !strings.Contains(decoded.Output.Text, string(selected.ID())) || !strings.Contains(decoded.Output.Text, "Keep it empty") {
					t.Fatalf("notice=%+v", decoded)
				}
				output.Reset()
				h.stdin = bytes.NewReader(payload)
				if err = h.Run(provider, event); err != nil {
					t.Fatal(err)
				}
				if output.Len() != 0 {
					t.Fatal("unchanged notice repeated")
				}
				after, _ := os.ReadFile(path)
				if !bytes.Equal(original, after) {
					t.Fatal("provider transcript changed")
				}
			})
		}
	}
}
