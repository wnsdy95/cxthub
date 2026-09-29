package capture

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func stagingSourceRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "-c", "core.hooksPath=/dev/null", "init", "-q", "-b", "main", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}
func stagingNativeFile(t *testing.T, home, cwd, id string, provider domain.ProviderKind) string {
	t.Helper()
	var path string
	var row any
	if provider == domain.ProviderClaude {
		path = filepath.Join(home, ".claude", "projects", providerfs.EncodeCwd(cwd), id+".jsonl")
		row = map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "message": map[string]string{"role": "user", "content": "synthetic"}}
	} else {
		path = filepath.Join(home, ".codex", "sessions", "2026", "09", "30", "rollout-2026-09-30T00-00-00-"+id+".jsonl")
		row = map[string]any{"type": "session_meta", "payload": map[string]string{"id": id, "cwd": cwd}}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(row)
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestStagingSourcesEnumeratesBothProvidersAndEveryEligibleSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := stagingSourceRepo(t)
	other := stagingSourceRepo(t)
	const a = "11111111-1111-4111-8111-111111111111"
	const b = "22222222-2222-4222-8222-222222222222"
	const c = "33333333-3333-4333-8333-333333333333"
	const d = "44444444-4444-4444-8444-444444444444"
	one := stagingNativeFile(t, home, root, a, domain.ProviderClaude)
	two := stagingNativeFile(t, home, root, b, domain.ProviderClaude)
	three := stagingNativeFile(t, home, root, c, domain.ProviderCodex)
	stagingNativeFile(t, home, other, d, domain.ProviderCodex)
	t.Setenv("CODEX_THREAD_ID", c) // An inherited app identity must not narrow add .
	result, err := ResolveStagingSources(context.Background(), root, nil)
	if err != nil || len(result.Sessions) != 3 || result.Selection != "all_eligible_in_worktree" {
		t.Fatal(result, err)
	}
	paths := map[string]bool{}
	for _, source := range result.Sessions {
		paths[source.Path] = true
	}
	for _, path := range []string{one, two, three} {
		if !paths[path] {
			t.Fatal("missing eligible source", path)
		}
	}
	result, err = ResolveStagingSources(context.Background(), root, []domain.ProviderKind{domain.ProviderClaude})
	if err != nil || len(result.Sessions) != 2 {
		t.Fatal("explicit provider selected latest only", result, err)
	}
	if err := providerfs.MarkSuperseded(root, one); err != nil {
		t.Fatal(err)
	}
	if err := providerfs.RecordMaterialized(root, three); err != nil {
		t.Fatal(err)
	}
	result, err = ResolveStagingSources(context.Background(), root, nil)
	if err != nil || len(result.Sessions) != 1 || result.Sessions[0].Path != two {
		t.Fatal("capture ledger ignored", result, err)
	}
	if err := os.Symlink(two, filepath.Join(filepath.Dir(two), d+".jsonl")); err != nil {
		t.Fatal(err)
	}
	result, err = ResolveStagingSources(context.Background(), root, nil)
	if err != nil || len(result.Sessions) != 1 {
		t.Fatal("symlink captured", result, err)
	}
}

func TestStagingSourcesRejectsInvalidIdentityAndCorruptLedger(t *testing.T) {
	for _, failure := range []string{"identity", "ledger", "unreadable-source", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			root := stagingSourceRepo(t)
			path := stagingNativeFile(t, home, root, "11111111-1111-4111-8111-111111111111", domain.ProviderClaude)
			switch failure {
			case "identity":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw = []byte(strings.ReplaceAll(string(raw), "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"))
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "ledger":
				if err := os.WriteFile(filepath.Join(root, ".cxt", "session-ledger.json"), []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unreadable-source":
				if err := os.WriteFile(path, []byte(`{"type":`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if failure == "cancelled" {
				cancel()
			}
			_, err := ResolveStagingSources(ctx, root, nil)
			if err == nil || errors.Is(err, domain.ErrNoActiveSession) {
				t.Fatal("selection failure reported as empty source set", err)
			}
		})
	}
}

func TestStagingSourcesExactWorktreeIncludesSubdirectoryButExcludesNestedRepo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := stagingSourceRepo(t)
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	const a = "11111111-1111-4111-8111-111111111111"
	const b = "22222222-2222-4222-8222-222222222222"
	source := stagingNativeFile(t, home, sub, a, domain.ProviderClaude)
	nested := filepath.Join(root, "nested")
	if out, err := exec.Command("git", "-c", "core.hooksPath=/dev/null", "init", "-q", nested).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	stagingNativeFile(t, home, nested, b, domain.ProviderCodex)
	result, err := ResolveStagingSources(context.Background(), root, nil)
	if err != nil || len(result.Sessions) != 1 || result.Sessions[0].Path != source {
		t.Fatal(result, err)
	}
}

func TestStagingSourcesRejectsAmbiguousCopiesAndDistinguishesAbsence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := stagingSourceRepo(t)
	if _, err := ResolveStagingSources(context.Background(), root, nil); !errors.Is(err, domain.ErrNoActiveSession) {
		t.Fatal(err)
	}
	const id = "11111111-1111-4111-8111-111111111111"
	path := stagingNativeFile(t, home, root, id, domain.ProviderCodex)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := filepath.Join(filepath.Dir(path), "rollout-2026-09-30T01-00-00-"+id+".jsonl")
	if err := os.WriteFile(duplicate, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveStagingSources(context.Background(), root, nil); err == nil || errors.Is(err, domain.ErrNoActiveSession) {
		t.Fatal("ambiguous copy accepted", err)
	}
}

func TestStagingSourcesScopesCodexBeforeValidatingUnrelatedIdentity(t *testing.T) {
	for _, damage := range []string{"filename-mismatch", "invalid-id", "missing-id"} {
		t.Run(damage, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			root, outside := stagingSourceRepo(t), stagingSourceRepo(t)
			const id = "11111111-1111-4111-8111-111111111111"
			const otherID = "22222222-2222-4222-8222-222222222222"
			wanted := stagingNativeFile(t, home, root, id, domain.ProviderCodex)
			unrelated := stagingNativeFile(t, home, outside, otherID, domain.ProviderCodex)
			brokenID := id
			if damage == "invalid-id" {
				brokenID = "invalid/session"
			} else if damage == "missing-id" {
				brokenID = ""
			}
			raw, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": brokenID, "cwd": outside}})
			if err := os.WriteFile(unrelated, append(raw, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := ResolveStagingSources(context.Background(), root, []domain.ProviderKind{domain.ProviderCodex})
			if err != nil || !result.Complete || len(result.Gaps) != 0 || len(result.Sessions) != 1 || result.Sessions[0].Path != wanted {
				t.Fatal("unrelated corrupt identity blocked current worktree", result, err)
			}
			// The same identity defect inside this worktree must still block add.
			raw, _ = json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": brokenID, "cwd": root}})
			if err := os.WriteFile(unrelated, append(raw, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			result, err = ResolveStagingSources(context.Background(), root, []domain.ProviderKind{domain.ProviderCodex})
			if err == nil || errors.Is(err, domain.ErrNoActiveSession) || result.Complete || len(result.Gaps) != 1 || result.Gaps[0].Path != unrelated {
				t.Fatal("in-scope corrupt identity silently omitted", result, err)
			}
		})
	}
}

func TestStagingSourcesReportsUnclassifiableCodexCoverage(t *testing.T) {
	for _, metadata := range []string{
		`{"type":"session_meta","payload":{"id":"11111111-1111-4111-8111-111111111111"}}`,
		`{"type":"session_meta","payload":`,
		`{"type":"not_session_meta","payload":{"id":"11111111-1111-4111-8111-111111111111","cwd":"/not/this/repo"}}`,
		`{"type":"session_meta","payload":{"id":123,"cwd":"/not/this/repo"}}`,
	} {
		t.Run(metadata, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			root := stagingSourceRepo(t)
			path := stagingNativeFile(t, home, root, "11111111-1111-4111-8111-111111111111", domain.ProviderCodex)
			if err := os.WriteFile(path, []byte(metadata+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := ResolveStagingSources(context.Background(), root, []domain.ProviderKind{domain.ProviderCodex})
			if err == nil || errors.Is(err, domain.ErrNoActiveSession) || result.Complete || len(result.Gaps) != 1 || result.Gaps[0].Provider != domain.ProviderCodex || result.Gaps[0].Path != path || !strings.Contains(err.Error(), "coverage incomplete") {
				t.Fatal("unclassified source presented as complete/absent", result, err)
			}
		})
	}
}
