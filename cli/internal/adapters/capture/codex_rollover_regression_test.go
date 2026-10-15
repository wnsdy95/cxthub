package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func readRolloverRegressionFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCodexRolloverEndPreservesReadOnlyFollowedSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := stagingSourceRepo(t)
	old := writeRollover(t, home, cwd, "old-"+rolloverSession, "2026-07-31T00:00:00Z")
	if err := TrackAppSession(cwd, domain.ProviderCodex, rolloverSession, old); err != nil {
		t.Fatal(err)
	}
	root, worktree, _ := appSessionRoots(context.Background(), cwd)
	registry := filepath.Join(root, appSessionRelativePath(domain.ProviderCodex, worktree, rolloverSession))
	before := readRolloverRegressionFile(t, registry)
	current := writeRollover(t, home, cwd, "new-"+rolloverSession+"_"+rolloverInstance, "2026-10-09T00:00:00Z")
	if sessions := ActiveAppSessions(cwd); len(sessions) != 1 || sessions[0].Path != current {
		t.Fatalf("rollover was not followed: %+v", sessions)
	}
	if !bytes.Equal(before, readRolloverRegressionFile(t, registry)) {
		t.Fatal("discovery rewrote the registry or heartbeat")
	}
	EndAppSession(cwd, domain.ProviderCodex, rolloverSession, old)
	if !bytes.Equal(before, readRolloverRegressionFile(t, registry)) {
		t.Fatal("old end changed the read-only-followed registry")
	}
	if sessions := ActiveAppSessions(cwd); len(sessions) != 1 || sessions[0].Path != current {
		t.Fatalf("old end removed the current generation: %+v", sessions)
	}
	if err := TrackAppSession(cwd, domain.ProviderCodex, rolloverSession, current); err != nil {
		t.Fatal(err)
	}
	EndAppSession(cwd, domain.ProviderCodex, rolloverSession, current)
	if _, err := os.Stat(registry); !os.IsNotExist(err) {
		t.Fatalf("current end did not remove its registry: %v", err)
	}
}

func TestCodexRolloverAcrossSubdirectoriesPreservesGeneration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := stagingSourceRepo(t)
	subdir := filepath.Join(cwd, "subdir")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	old := writeRollover(t, home, cwd, "old-"+rolloverSession, "2026-07-31T00:00:00Z")
	if err := TrackAppSession(cwd, domain.ProviderCodex, rolloverSession, old); err != nil {
		t.Fatal(err)
	}
	current := writeRollover(t, home, subdir, "new-"+rolloverSession+"_"+rolloverInstance, "2026-10-09T00:00:00Z")
	before := map[string][]byte{old: readRolloverRegressionFile(t, old), current: readRolloverRegressionFile(t, current)}
	ctx := context.Background()
	for _, directory := range []string{cwd, subdir} {
		for name, locate := range map[string]func() (string, error){
			"exact":   func() (string, error) { return NewCodexCapture().LocateSession(ctx, directory, rolloverSession) },
			"active":  func() (string, error) { return NewCodexCapture().LocateActiveSession(ctx, directory) },
			"command": func() (string, error) { return LocateCodexCommandSession(ctx, directory, rolloverSession) },
			"registry": func() (string, error) {
				return LocateRegisteredAppSession(directory, domain.ProviderCodex, rolloverSession)
			},
		} {
			if got, err := locate(); err != nil || got != current {
				t.Fatalf("%s from %s selected %q: %v", name, directory, got, err)
			}
		}
	}
	if err := TrackAppSession(subdir, domain.ProviderCodex, rolloverSession, current); err != nil {
		t.Fatal(err)
	}
	if err := TrackAppSession(cwd, domain.ProviderCodex, rolloverSession, old); !errors.Is(err, ErrSessionIdentityMismatch) {
		t.Fatalf("old hook accepted after new registration: %v", err)
	}
	if sessions := ActiveAppSessions(cwd); len(sessions) != 1 || sessions[0].Path != current {
		t.Fatalf("registry did not retain the new generation: %+v", sessions)
	}
	sources, err := ResolveStagingSources(ctx, cwd, []domain.ProviderKind{domain.ProviderCodex})
	if err != nil || !sources.Complete || len(sources.Sessions) != len(before) {
		t.Fatalf("same-worktree family staging failed: %+v %v", sources, err)
	}
	seen := make(map[string]bool)
	for _, source := range sources.Sessions {
		if _, ok := before[source.Path]; !ok || seen[source.Path] || source.SessionID != rolloverSession || source.Provider != domain.ProviderCodex {
			t.Fatalf("unexpected or duplicate source: %+v", source)
		}
		seen[source.Path] = true
	}
	for path, raw := range before {
		if !bytes.Equal(raw, readRolloverRegressionFile(t, path)) {
			t.Fatalf("native source changed: %s", path)
		}
	}
}

func TestAppSessionDiscoveryIgnoresInvalidRegistryWithoutMutation(t *testing.T) {
	for _, scenario := range []string{"expired", "future", "malformed", "missing-source", "unsupported-version"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			cwd := stagingSourceRepo(t)
			path := writeRollover(t, home, cwd, "old-"+rolloverSession, "2026-07-31T00:00:00Z")
			if err := TrackAppSession(cwd, domain.ProviderCodex, rolloverSession, path); err != nil {
				t.Fatal(err)
			}
			root, worktree, _ := appSessionRoots(context.Background(), cwd)
			registry := filepath.Join(root, appSessionRelativePath(domain.ProviderCodex, worktree, rolloverSession))
			var state appSessionFile
			if err := json.Unmarshal(readRolloverRegressionFile(t, registry), &state); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "expired":
				state.UpdatedAt = time.Now().Add(-appSessionTTL - time.Hour)
			case "future":
				state.UpdatedAt = time.Now().Add(time.Hour)
			case "missing-source":
				state.Path = filepath.Join(filepath.Dir(path), "rollout-missing-"+rolloverSession+".jsonl")
			case "unsupported-version":
				state.Version++
			}
			before, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "malformed" {
				before = []byte("{broken")
			}
			if err := os.WriteFile(registry, before, 0600); err != nil {
				t.Fatal(err)
			}
			if sessions := ActiveAppSessions(cwd); len(sessions) != 0 {
				t.Fatalf("invalid registry exposed: %+v", sessions)
			}
			if got, err := LocateRegisteredAppSession(cwd, domain.ProviderCodex, rolloverSession); !errors.Is(err, domain.ErrNoActiveSession) || got != "" {
				t.Fatalf("invalid registry resolved: %q %v", got, err)
			}
			if !bytes.Equal(before, readRolloverRegressionFile(t, registry)) {
				t.Fatal("discovery mutated the invalid registry")
			}
			if err := TrackAppSession(cwd, domain.ProviderCodex, rolloverSession, path); err != nil {
				t.Fatal(err)
			}
			refreshed := readRolloverRegressionFile(t, registry)
			if sessions := ActiveAppSessions(cwd); len(sessions) != 1 || sessions[0].Path != path {
				t.Fatalf("fresh registration was lost: %+v", sessions)
			}
			if !bytes.Equal(refreshed, readRolloverRegressionFile(t, registry)) {
				t.Fatal("discovery rewrote the fresh registration")
			}
		})
	}
}

func TestCodexRolloverValidationRejectsLinkedWorktreeFamily(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	primary := stagingSourceRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, args := range [][]string{
		{"-c", "user.name=cxt test", "-c", "user.email=cxt@example.test", "-c", "commit.gpgSign=false", "commit", "--allow-empty", "-m", "initial"},
		{"worktree", "add", "--detach", linked, "HEAD"},
	} {
		command := exec.CommandContext(ctx, "git", append([]string{"-C", primary, "-c", "core.hooksPath=/dev/null"}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", args, output, err)
		}
	}
	old := writeRollover(t, home, primary, "old-"+rolloverSession, "2026-07-31T00:00:00Z")
	if err := TrackAppSession(primary, domain.ProviderCodex, rolloverSession, old); err != nil {
		t.Fatal(err)
	}
	current := writeRollover(t, home, linked, "new-"+rolloverSession+"_"+rolloverInstance, "2026-10-09T00:00:00Z")
	root, worktree, _ := appSessionRoots(ctx, primary)
	registry := filepath.Join(root, appSessionRelativePath(domain.ProviderCodex, worktree, rolloverSession))
	before := readRolloverRegressionFile(t, registry)
	for _, candidate := range []struct{ cwd, path string }{{primary, old}, {linked, current}} {
		if err := NewCodexCapture().ValidateCurrentSession(ctx, candidate.path); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("cross-worktree generation accepted: %s %v", candidate.path, err)
		}
		if got, err := LocateCodexCommandSession(ctx, candidate.cwd, rolloverSession); err == nil || !strings.Contains(err.Error(), "ambiguous") || got != "" {
			t.Fatalf("cross-worktree command resolved: %q %v", got, err)
		}
		if err := TrackAppSession(candidate.cwd, domain.ProviderCodex, rolloverSession, candidate.path); err == nil {
			t.Fatal("ambiguous family updated the registry")
		}
	}
	if sessions := ActiveAppSessions(primary); len(sessions) != 0 {
		t.Fatalf("ambiguous family exposed as active: %+v", sessions)
	}
	EndAppSession(primary, domain.ProviderCodex, rolloverSession, old)
	if !bytes.Equal(before, readRolloverRegressionFile(t, registry)) {
		t.Fatal("ambiguous family mutated the registry")
	}
}
