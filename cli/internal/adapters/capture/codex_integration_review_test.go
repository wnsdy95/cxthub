package capture

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestCodexRegisteredRolloverRemainsLiveAtCaptureBaseline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := stagingSourceRepo(t)
	old := writeRollover(t, home, cwd, "old-"+rolloverSession, "2026-07-31T00:00:00Z")
	if err := TrackAppSession(cwd, domain.ProviderCodex, rolloverSession, old); err != nil {
		t.Fatal(err)
	}
	current := writeRollover(t, home, cwd, "new-"+rolloverSession+"_"+rolloverInstance, "2026-10-09T00:00:00Z")
	if err := providerfs.RecordMaterialized(cwd, current); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(cwd, ".cxt", "session-ledger.json")
	before := readRolloverRegressionFile(t, ledgerPath)
	if sessions := ActiveAppSessions(cwd); len(sessions) != 1 || sessions[0].Path != current {
		t.Fatalf("capture baseline hid the live generation: %+v", sessions)
	}
	if _, err := LocateRegisteredAppSession(cwd, domain.ProviderCodex, rolloverSession); !errors.Is(err, domain.ErrNoActiveSession) {
		t.Fatalf("baseline became command-capture eligible: %v", err)
	}
	if !bytes.Equal(before, readRolloverRegressionFile(t, ledgerPath)) {
		t.Fatal("liveness lookup changed the capture baseline")
	}
	if err := providerfs.MarkSuperseded(cwd, current); err != nil {
		t.Fatal(err)
	}
	if sessions := ActiveAppSessions(cwd); len(sessions) != 0 {
		t.Fatalf("permanently retired generation became live: %+v", sessions)
	}
}

func TestCodexStagingIgnoresCaptureExcludedArchives(t *testing.T) {
	for _, scenario := range []string{"malformed-retired", "duplicate-retired", "missing-timestamp-baseline"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			cwd := stagingSourceRepo(t)
			archived := writeRollover(t, home, cwd, "old-"+rolloverSession, "")
			name := "new-" + rolloverSession + "_" + rolloverInstance
			if scenario == "duplicate-retired" {
				name = "new-" + rolloverSession
			}
			current := writeRollover(t, home, cwd, name, "2026-10-09T00:00:00Z")
			if scenario == "missing-timestamp-baseline" {
				if err := providerfs.RecordMaterialized(cwd, archived); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := providerfs.MarkSuperseded(cwd, archived); err != nil {
					t.Fatal(err)
				}
				if scenario == "malformed-retired" {
					if err := os.WriteFile(archived, []byte("{broken\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := readRolloverRegressionFile(t, archived)
			result, err := ResolveStagingSources(context.Background(), cwd, []domain.ProviderKind{domain.ProviderCodex})
			if err != nil || !result.Complete || len(result.Sessions) != 1 || result.Sessions[0].Path != current {
				t.Fatalf("excluded archive blocked eligible staging: %+v %v", result, err)
			}
			if !bytes.Equal(before, readRolloverRegressionFile(t, archived)) {
				t.Fatal("staging changed the excluded archive")
			}
		})
	}
}

func TestCodexCurrentGenerationAcceptsCanonicalHomeSpelling(t *testing.T) {
	realHome := t.TempDir()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(realHome, home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	cwd := stagingSourceRepo(t)
	old := writeRollover(t, home, cwd, "old-"+rolloverSession, "2026-07-31T00:00:00Z")
	current := writeRollover(t, home, cwd, "new-"+rolloverSession+"_"+rolloverInstance, "2026-10-09T00:00:00Z")
	canonicalCurrent, err := filepath.EvalSymlinks(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateHookSession(cwd, domain.ProviderCodex, rolloverSession, canonicalCurrent); err != nil {
		t.Fatalf("canonical spelling of the current rollout rejected: %v", err)
	}
	canonicalOld, err := filepath.EvalSymlinks(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateHookSession(cwd, domain.ProviderCodex, rolloverSession, canonicalOld); !errors.Is(err, ErrSessionIdentityMismatch) {
		t.Fatalf("canonical spelling bypassed the generation guard: %v", err)
	}
}

func TestCodexActiveSessionDoesNotHideAmbiguityBehindSibling(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := stagingSourceRepo(t)
	writeRollover(t, home, cwd, "old-"+rolloverSession, "2026-10-09T00:00:00Z")
	writeRollover(t, home, cwd, "new-"+rolloverSession+"_"+rolloverInstance, "2026-10-09T00:00:00Z")
	stagingNativeFile(t, home, cwd, "33333333-3333-4333-8333-333333333333", domain.ProviderCodex)
	if path, err := NewCodexCapture().LocateActiveSession(context.Background(), cwd); err == nil || errors.Is(err, domain.ErrNoActiveSession) || path != "" {
		t.Fatalf("ambiguous generation silently selected a sibling: %q %v", path, err)
	}
}
