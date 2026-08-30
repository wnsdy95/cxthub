package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeCodexHooksIncludesDesktopSessionEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	if err := os.WriteFile(path, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"user-hook"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := mergeAgentHooks(path, "codex")
	if err != nil || !changed {
		t.Fatalf("first merge changed=%v err=%v", changed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		entries := root.Hooks[event]
		found := false
		for _, raw := range entries {
			if strings.Contains(string(raw), "cxt hook --provider codex --event "+event) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s cxt hook missing: %s", event, data)
		}
	}
	if !strings.Contains(string(data), "user-hook") {
		t.Fatal("existing user hook was overwritten")
	}
	if changed, err := mergeAgentHooks(path, "codex"); err != nil || changed {
		t.Fatalf("idempotent merge changed=%v err=%v", changed, err)
	}
}
