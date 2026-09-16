package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestCommandIdentityAcrossWorktreesRejectsFalseSources(t *testing.T) {
	const parent = "11111111-1111-4111-8111-111111111111"
	const child = "22222222-2222-4222-8222-222222222222"
	for _, scenario := range []string{"unregistered", "poisoned-registry", "foreign-repo", "metadata-mismatch", "duplicate", "excluded"} {
		t.Run(scenario, func(t *testing.T) {
			cwd, _, _, _, _ := historyFixture(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CXT_WRAPPED", "")
			t.Setenv("CODEX_THREAD_ID", parent)
			owned := writeCodexRollout(t, home, cwd, parent, time.Now())
			linked := filepath.Join(t.TempDir(), "linked")
			runLifecycleGit(t, cwd, "worktree", "add", "-b", "feature", linked)
			newer := writeCodexRollout(t, home, linked, child, time.Now().Add(time.Minute))
			switch scenario {
			case "poisoned-registry":
				if err := capture.TrackAppSession(cwd, domain.ProviderCodex, parent, owned); err != nil {
					t.Fatal(err)
				}
				files, err := filepath.Glob(filepath.Join(cwd, ".cxt", "app-sessions", "*.json"))
				if err != nil || len(files) != 1 {
					t.Fatalf("registry fixture: %v %v", files, err)
				}
				raw, err := os.ReadFile(files[0])
				if err != nil {
					t.Fatal(err)
				}
				var record map[string]any
				if err := json.Unmarshal(raw, &record); err != nil {
					t.Fatal(err)
				}
				record["path"] = newer // Legacy parent ID overwritten by a child's hook.
				raw, err = json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(files[0], raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-repo":
				linked, _, _, _, _ = historyFixture(t)
			case "metadata-mismatch":
				raw, err := os.ReadFile(owned)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(owned, bytes.ReplaceAll(raw, []byte(parent), []byte(child)), 0600); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				raw, err := os.ReadFile(owned)
				if err != nil {
					t.Fatal(err)
				}
				copy := filepath.Join(home, ".codex", "sessions", "2030", "01", "01", "rollout-copy-"+parent+".jsonl")
				if err := os.MkdirAll(filepath.Dir(copy), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(copy, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "excluded":
				if err := providerfs.RecordMaterialized(cwd, owned); err != nil {
					t.Fatal(err)
				}
			}
			target, err := commandCapture(context.Background(), linked, "codex")
			if scenario == "unregistered" || scenario == "poisoned-registry" {
				if err != nil || target.SessionPath != owned {
					t.Fatalf("exact command failed: %+v %v", target, err)
				}
			} else if err == nil || target.SessionPath != "" {
				t.Fatalf("invalid source accepted: %+v %v", target, err)
			}
			if sessions := capture.ActiveAppSessions(linked); len(sessions) != 0 {
				t.Fatal("command lookup broadened background capture")
			}
		})
	}
}
