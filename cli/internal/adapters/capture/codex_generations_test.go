package capture

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
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

func TestCodexRolloverUsesNativeGenerationNotMtime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := stagingSourceRepo(t)
	old := writeRollover(t, home, cwd, "old-"+rolloverSession, "2026-07-31T02:40:24Z")
	if err := TrackAppSession(cwd, domain.ProviderCodex, rolloverSession, old); err != nil {
		t.Fatal(err)
	}
	root, worktree, _ := appSessionRoots(context.Background(), cwd)
	registry := filepath.Join(root, appSessionRelativePath(domain.ProviderCodex, worktree, rolloverSession))
	before, _ := os.ReadFile(registry)
	current := writeRollover(t, home, cwd, "new-"+rolloverSession+"_"+rolloverInstance, "2026-10-09T23:19:52.995Z")
	oldBytes, _ := os.ReadFile(old)
	currentBytes, _ := os.ReadFile(current)
	if err := os.Chtimes(old, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, locate := range map[string]func() (string, error){
		"exact":    func() (string, error) { return NewCodexCapture().LocateSession(ctx, cwd, rolloverSession) },
		"command":  func() (string, error) { return LocateCodexCommandSession(ctx, cwd, rolloverSession) },
		"registry": func() (string, error) { return LocateRegisteredAppSession(cwd, domain.ProviderCodex, rolloverSession) },
		"active":   func() (string, error) { return NewCodexCapture().LocateActiveSession(ctx, cwd) },
	} {
		if got, err := locate(); err != nil || got != current {
			t.Fatalf("%s selected %q: %v", name, got, err)
		}
	}
	after, _ := os.ReadFile(registry)
	if string(before) != string(after) {
		t.Fatal("discovery changed the lifecycle heartbeat")
	}
	if _, err := LocateCodexCommandSession(ctx, cwd, rolloverInstance); !errors.Is(err, domain.ErrNoActiveSession) {
		t.Fatalf("instance ID became session: %v", err)
	}
	if err := TrackAppSession(cwd, domain.ProviderCodex, rolloverSession, current); err != nil {
		t.Fatal(err)
	}
	if err := TrackAppSession(cwd, domain.ProviderCodex, rolloverSession, old); !errors.Is(err, ErrSessionIdentityMismatch) {
		t.Fatalf("late old hook accepted: %v", err)
	}
	EndAppSession(cwd, domain.ProviderCodex, rolloverSession, old)
	if sessions := ActiveAppSessions(cwd); len(sessions) != 1 || sessions[0].Path != current {
		t.Fatalf("old end deleted current registry: %+v", sessions)
	}
	sources, err := ResolveStagingSources(ctx, cwd, []domain.ProviderKind{domain.ProviderCodex})
	if err != nil || !sources.Complete || len(sources.Sessions) != 2 {
		t.Fatalf("historical source lost: %+v %v", sources, err)
	}
	for path, want := range map[string][]byte{old: oldBytes, current: currentBytes} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(want) {
			t.Fatalf("native source changed: %s %v", path, err)
		}
	}
	if err := providerfs.RecordMaterialized(root, current); err != nil {
		t.Fatal(err)
	}
	if got, err := NewCodexCapture().LocateSession(ctx, cwd, rolloverSession); !errors.Is(err, domain.ErrNoActiveSession) {
		t.Fatalf("excluded current resurrected old: %q %v", got, err)
	}
	if err := os.Rename(current, current+".superseded"); err != nil {
		t.Fatal(err)
	}
	if got, err := NewCodexCapture().LocateSession(ctx, cwd, rolloverSession); !errors.Is(err, domain.ErrNoActiveSession) {
		t.Fatalf("retired generation resurrected old: %q %v", got, err)
	}
}

func TestCodexGenerationAmbiguityFailsClosed(t *testing.T) {
	for _, scenario := range []string{"same-instant", "missing-timestamp", "ordinary-copies", "cross-worktree", "malformed-suffix"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			cwd := stagingSourceRepo(t)
			oldTimestamp, newTimestamp := "2026-07-31T02:40:24Z", "2026-10-09T23:19:52.995Z"
			if scenario == "same-instant" {
				oldTimestamp = "2026-10-09T16:19:52.995-07:00"
			}
			if scenario == "missing-timestamp" {
				oldTimestamp = ""
			}
			old := writeRollover(t, home, cwd, "old-"+rolloverSession, oldTimestamp)
			otherCwd := cwd
			if scenario == "cross-worktree" {
				otherCwd = stagingSourceRepo(t)
			}
			name := "new-" + rolloverSession + "_" + rolloverInstance
			if scenario == "malformed-suffix" {
				name = "new-" + rolloverSession + "_invalid"
			}
			newPath := writeRollover(t, home, otherCwd, name, newTimestamp)
			if scenario == "malformed-suffix" {
				if err := ValidateHookSession(cwd, domain.ProviderCodex, rolloverSession, newPath); !errors.Is(err, ErrSessionIdentityMismatch) {
					t.Fatal("malformed generation accepted", err)
				}
				return
			}
			if scenario == "ordinary-copies" {
				writeRollover(t, home, cwd, "copy-"+rolloverSession, "2026-08-01T00:00:00Z")
			}
			if scenario == "cross-worktree" {
				left, _ := readCodexSessionHeader(old)
				right, _ := readCodexSessionHeader(newPath)
				if _, err := selectCodexSession([]codexSessionCandidate{{path: old, header: left, worktree: cwd}, {path: newPath, header: right, worktree: otherCwd}}); err == nil {
					t.Fatal("cross-worktree ambiguity accepted")
				}
				return
			}
			if _, err := NewCodexCapture().LocateSession(context.Background(), cwd, rolloverSession); err == nil || errors.Is(err, domain.ErrNoActiveSession) {
				t.Fatal("ambiguous generation accepted", err)
			}
		})
	}
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
