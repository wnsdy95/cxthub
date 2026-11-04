package capture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestProviderSessionInventoryKeepsExcludedFilesOutOfActiveSelection(test *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		test.Run(provider, func(test *testing.T) {
			home, cwd := test.TempDir(), test.TempDir()
			test.Setenv("HOME", home)
			test.Setenv("CODEX_HOME", "")
			const sessionID = "11111111-1111-4111-8111-111111111111"
			directory := filepath.Join(home, ".claude", "projects", providerfs.EncodeCwd(cwd))
			name := sessionID + ".jsonl"
			raw := fmt.Sprintf("{\"type\":\"user\",\"sessionId\":%q,\"message\":{\"role\":\"user\",\"content\":\"inventory\"}}\n", sessionID)
			if provider == domain.ProviderCodex {
				directory = filepath.Join(home, ".codex", "sessions", "2026", "10", "30")
				name = "rollout-test-" + sessionID + ".jsonl"
				raw = fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q}}\n", sessionID, cwd)
			}
			if err := os.MkdirAll(directory, 0700); err != nil {
				test.Fatal(err)
			}
			path := filepath.Join(directory, name)
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				test.Fatal(err)
			}
			inventory := NewCommandSessionResolver(NewClaudeCapture(), NewCodexCapture())
			if selected, err := inventory.LocateActiveSession(context.Background(), cwd, provider); err != nil || selected != path {
				test.Fatal("eligible native session was not selected", selected, err)
			}
			if err := providerfs.RecordCaptureBaseline(cwd, path, int64(len(raw))); err != nil {
				test.Fatal(err)
			}
			if selected, err := inventory.LocateActiveSession(context.Background(), cwd, provider); !errors.Is(err, domain.ErrNoActiveSession) || selected != "" {
				test.Fatal("excluded native session became active", selected, err)
			}
			if files := inventory.SessionFilesForCwd(context.Background(), cwd, provider); !reflect.DeepEqual(files, []string{path}) {
				test.Fatal("isolation inventory incorrectly reused active filtering", files)
			}
		})
	}
}
