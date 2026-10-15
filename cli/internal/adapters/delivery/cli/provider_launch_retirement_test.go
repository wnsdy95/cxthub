package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestRetireOwnedCodexSessionVerifiesEveryGeneration(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const instanceID = "22222222-2222-4222-8222-222222222222"
	const siblingID = "33333333-3333-4333-8333-333333333333"
	dir := filepath.Join(home, ".codex", "sessions", "2026", "10", "09")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	fixtures := []struct {
		name       string
		filenameID string
		headerID   string
		cwd        string
		retire     bool
	}{
		{"ordinary", sessionID, sessionID, cwd, true},
		{"rotated", sessionID + "_" + instanceID, sessionID, cwd, true},
		{"mismatched", sessionID + "_" + siblingID, siblingID, cwd, false},
		{"missing-id", sessionID, "", cwd, false},
		{"sibling", siblingID, siblingID, cwd, false},
		{"instance-only", siblingID + "_" + sessionID, siblingID, cwd, false},
		{"other-cwd", sessionID, sessionID, t.TempDir(), false},
		{"nested-cwd", sessionID, sessionID, filepath.Join(cwd, "nested"), false},
	}
	contents := make(map[string]string)
	for _, fixture := range fixtures {
		path := filepath.Join(dir, "rollout-"+fixture.name+"-"+fixture.filenameID+".jsonl")
		raw := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q}}\n", fixture.headerID, fixture.cwd)
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		contents[path] = raw
		if fixture.retire {
			if err := providerfs.RecordCaptureBaseline(cwd, path, int64(len(raw))); err != nil {
				t.Fatal(err)
			}
		}
	}
	retireOwnedProviderSession(context.Background(), cwd, domain.ProviderCodex, sessionID)
	for _, fixture := range fixtures {
		original := filepath.Join(dir, "rollout-"+fixture.name+"-"+fixture.filenameID+".jsonl")
		path := original
		if fixture.retire {
			path += ".superseded"
			if _, err := os.Lstat(original); !os.IsNotExist(err) {
				t.Fatalf("owned generation was not retired: %q: %v", original, err)
			}
		} else if _, err := os.Lstat(original + ".superseded"); !os.IsNotExist(err) {
			t.Fatalf("unowned source was retired: %q: %v", original, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != contents[original] {
			t.Fatalf("source contents changed: %q: %v", path, err)
		}
		if got := providerfs.CaptureExcluded(cwd, original, int64(len(raw))+1); got != fixture.retire {
			t.Fatalf("retirement ledger for %q = %t, want %t", original, got, fixture.retire)
		}
	}
}

func TestCodexRetirementTargetRejectsUnverifiedHeaders(t *testing.T) {
	cwd := t.TempDir()
	const sessionID = "11111111-1111-4111-8111-111111111111"
	valid := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q}}\n", sessionID, cwd)
	for _, fixture := range []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"malformed", "{\"type\":\"session_meta\""},
		{"wrong-type", strings.Replace(valid, "session_meta", "response_item", 1)},
		{"wrong-id", strings.Replace(valid, sessionID, "22222222-2222-4222-8222-222222222222", 1)},
		{"wrong-cwd", strings.Replace(valid, cwd, filepath.Join(cwd, "nested"), 1)},
		{"missing-cwd", fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n", sessionID)},
		{"oversized", strings.Repeat(" ", 1<<20) + valid},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout-test-"+sessionID+".jsonl")
			if err := os.WriteFile(path, []byte(fixture.raw), 0600); err != nil {
				t.Fatal(err)
			}
			if validCodexRetirementTarget(cwd, sessionID, path) {
				t.Fatal("unverified retirement header accepted")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "rollout-test-"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if !validCodexRetirementTarget(cwd, sessionID, path) {
		t.Fatal("verified retirement header rejected")
	}
	link := filepath.Join(t.TempDir(), "rollout-link-"+sessionID+".jsonl")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if validCodexRetirementTarget(cwd, sessionID, link) {
		t.Fatal("symlink retirement target accepted")
	}
}
