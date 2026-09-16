package capture

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestTrackAppSessionRejectsMismatchBeforeReplacingGoodPointer(t *testing.T) {
	const parent = "11111111-1111-4111-8111-111111111111"
	const child = "22222222-2222-4222-8222-222222222222"
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		for _, mismatch := range []string{"filename", "metadata", "cwd"} {
			t.Run(string(provider)+"/"+mismatch, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				_, _, cwd, _ := newTestCoord(t)
				good := writeCoordinatorSession(t, home, cwd, provider, parent, time.Now())
				bad := writeCoordinatorSession(t, home, cwd, provider, child, time.Now())
				if err := TrackAppSession(cwd, provider, parent, good); err != nil {
					t.Fatal(err)
				}
				root, worktree, _ := appSessionRoots(context.Background(), cwd)
				registry := filepath.Join(root, appSessionRelativePath(provider, worktree, parent))
				before, err := os.ReadFile(registry)
				if err != nil {
					t.Fatal(err)
				}
				if mismatch == "metadata" {
					// Filename claims parent, native record still identifies child.
					raw, err := os.ReadFile(bad)
					if err != nil {
						t.Fatal(err)
					}
					bad = filepath.Join(filepath.Dir(good), "copy-"+parent+".jsonl")
					if err := os.WriteFile(bad, raw, 0600); err != nil {
						t.Fatal(err)
					}
				} else if mismatch == "cwd" {
					_, _, other, _ := newTestCoord(t)
					bad = writeCoordinatorSession(t, home, other, provider, parent, time.Now())
				}
				if err := TrackAppSession(cwd, provider, parent, bad); !errors.Is(err, ErrSessionIdentityMismatch) {
					t.Fatalf("mismatch accepted: %v", err)
				}
				after, err := os.ReadFile(registry)
				if err != nil || string(before) != string(after) {
					t.Fatalf("good registry was replaced: %v", err)
				}
			})
		}
	}
}

func TestOpaqueAppSessionAliasCannotRebind(t *testing.T) {
	const alias = "opaque-hook-id"
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		t.Run(string(provider), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			_, _, cwd, _ := newTestCoord(t)
			good := writeCoordinatorSession(t, home, cwd, provider, "11111111-1111-4111-8111-111111111111", time.Now())
			bad := writeCoordinatorSession(t, home, cwd, provider, "22222222-2222-4222-8222-222222222222", time.Now())
			if err := TrackAppSession(cwd, provider, alias, good); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{good, ""} {
				if err := TrackAppSession(cwd, provider, alias, path); err != nil {
					t.Fatalf("valid alias refresh: %v", err)
				}
			}
			if err := TrackAppSession(cwd, provider, alias, bad); !errors.Is(err, ErrSessionIdentityMismatch) {
				t.Fatalf("opaque alias rebound: %v", err)
			}
			if sessions := ActiveAppSessions(cwd); len(sessions) != 1 || sessions[0].Path != good || sessions[0].SessionID != alias {
				t.Fatalf("alias lost binding: %+v", sessions)
			}
		})
	}
}

func TestAppSessionRegistryIsWorktreeScopedAndNonDestructive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	primary := filepath.Join(t.TempDir(), "primary")
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.MkdirAll(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(cwd string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(primary, "init", "-b", "main")
	git(primary, "config", "user.name", "cxt test")
	git(primary, "config", "user.email", "cxt@example.test")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	git(primary, "config", "core.hooksPath", hooks)
	git(primary, "config", "gc.auto", "0")
	git(primary, "commit", "--allow-empty", "-m", "initial")
	git(primary, "worktree", "add", "-b", "feature/app", linked)
	if err := os.Mkdir(filepath.Join(primary, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(primary, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const primaryID = "claude-app-thread"
	const linkedID = "codex-app-thread"
	const primaryNativeID = "11111111-1111-4111-8111-111111111111"
	const linkedNativeID = "22222222-2222-4222-8222-222222222222"
	primaryDir := filepath.Join(home, ".claude", "projects", providerfs.EncodeCwd(primary))
	linkedDir := filepath.Join(home, ".claude", "projects", providerfs.EncodeCwd(linked))
	if err := os.MkdirAll(primaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(linkedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	primaryPath := filepath.Join(primaryDir, primaryNativeID+".jsonl")
	linkedPath := filepath.Join(linkedDir, linkedNativeID+".jsonl")
	if err := os.WriteFile(primaryPath, []byte("primary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(linkedPath, []byte("linked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := TrackAppSession(primary, domain.ProviderClaude, primaryID, primaryPath); err != nil {
		t.Fatal(err)
	}
	if err := TrackAppSession(linked, domain.ProviderClaude, linkedID, linkedPath); err != nil {
		t.Fatal(err)
	}
	if got := ActiveAppSessions(primary); len(got) != 1 || got[0].SessionID != primaryID {
		t.Fatalf("primary sessions = %+v", got)
	}
	if got := ActiveAppSessions(linked); len(got) != 1 || got[0].SessionID != linkedID {
		t.Fatalf("linked sessions = %+v", got)
	}
	if _, err := os.Lstat(filepath.Join(linked, ".cxt")); !os.IsNotExist(err) {
		t.Fatalf("linked worktree gained split state: %v", err)
	}
	EndAppSession(linked, domain.ProviderClaude, linkedID)
	if got := ActiveAppSessions(linked); len(got) != 0 {
		t.Fatalf("ended linked session remains active: %+v", got)
	}
	if data, err := os.ReadFile(linkedPath); err != nil || string(data) != "linked\n" {
		t.Fatalf("ending registry entry changed transcript: %q, %v", data, err)
	}
}
