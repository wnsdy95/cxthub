package capture

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// These archives are entirely synthetic. A retired wrapper binding and a
// capture-excluded archive are different states; broad inventory is not proof
// that a file owns the command currently running in a managed thread.
func TestCodexRetiredAuditEnumerationAndExactOwnership(t *testing.T) {
	for _, retirement := range []string{"ended-binding-only", "superseded-suffix", "superseded-ledger", "provider-archive"} {
		t.Run(retirement, func(t *testing.T) {
			fixtureHome := t.TempDir()
			t.Setenv("HOME", fixtureHome)
			cwd := stagingSourceRepo(t)
			const retiredID = "11111111-1111-4111-8111-111111111111"
			const liveID = "22222222-2222-4222-8222-222222222222"
			live := stagingNativeFile(t, fixtureHome, cwd, liveID, domain.ProviderCodex)
			retired := stagingNativeFile(t, fixtureHome, cwd, retiredID, domain.ProviderCodex)
			for id, path := range map[string]string{liveID: live, retiredID: retired} {
				if err := TrackAppSession(cwd, domain.ProviderCodex, id, path); err != nil {
					t.Fatal(err)
				}
			}
			retire, err := BindNativeWrapperSession(cwd, 100, retiredID)
			if err != nil {
				t.Fatal(err)
			}
			cleanup, err := BindNativeWrapperSession(cwd, 100, liveID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cleanup(); err != nil {
					t.Error(err)
				}
			})
			if err := retire(); err != nil {
				t.Fatal(err)
			}
			EndAppSession(cwd, domain.ProviderCodex, retiredID)
			switch retirement {
			case "superseded-suffix":
				if err := os.Rename(retired, retired+".superseded"); err != nil {
					t.Fatal(err)
				}
				retired += ".superseded"
			case "superseded-ledger":
				if err := providerfs.MarkSuperseded(cwd, retired); err != nil {
					t.Fatal(err)
				}
			case "provider-archive":
				archived := filepath.Join(fixtureHome, ".codex", "archived_sessions", filepath.Base(retired))
				if err := os.MkdirAll(filepath.Dir(archived), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(retired, archived); err != nil {
					t.Fatal(err)
				}
				retired = archived
			}
			// Retired data is deliberately newer, and grows after retirement. Neither
			// mtime nor growth may restore the removed exact wrapper binding.
			f, err := os.OpenFile(retired, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("{\"type\":\"event_msg\",\"payload\":{\"type\":\"synthetic\"}}\n"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			for path, stamp := range map[string]time.Time{live: time.Unix(1000, 0), retired: time.Unix(2000, 0)} {
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			before := map[string][]byte{}
			for _, path := range []string{live, retired} {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = raw
			}
			ctx := context.Background()
			broad := NewCodexCapture().SessionFilesForCwd(ctx, cwd)
			wantBroad := []string{live}
			if retirement == "ended-binding-only" || retirement == "superseded-ledger" {
				wantBroad = append(wantBroad, retired)
			}
			slices.Sort(broad)
			slices.Sort(wantBroad)
			if !slices.Equal(broad, wantBroad) {
				t.Fatalf("broad inventory=%v want=%v", broad, wantBroad)
			}
			staging, err := ResolveStagingSources(ctx, cwd, []domain.ProviderKind{domain.ProviderCodex})
			wantStaging := map[string]StagingSource{
				live: {Provider: domain.ProviderCodex, SessionID: liveID, Path: live},
			}
			if retirement == "ended-binding-only" {
				wantStaging[retired] = StagingSource{Provider: domain.ProviderCodex, SessionID: retiredID, Path: retired}
			}
			if err != nil || !staging.Complete || len(staging.Sessions) != len(wantStaging) {
				t.Fatalf("staging=%+v error=%v", staging, err)
			}
			for _, source := range staging.Sessions {
				want, ok := wantStaging[source.Path]
				if !ok || source != want {
					t.Fatalf("unexpected or duplicate staging source=%+v want=%+v", source, want)
				}
				delete(wantStaging, source.Path)
			}
			active, err := NewCodexCapture().LocateActiveSession(ctx, cwd)
			wantActive := live
			if retirement == "ended-binding-only" {
				wantActive = retired
			}
			if err != nil || active != wantActive {
				t.Fatalf("latest eligible=%q error=%v", active, err)
			}
			if owned, err := NativeWrapperSession(cwd, 100, liveID); err != nil || owned != liveID {
				t.Fatalf("live binding=%q error=%v", owned, err)
			}
			if _, err := NativeWrapperSession(cwd, 100, retiredID); !errors.Is(err, domain.ErrNoActiveSession) {
				t.Fatalf("retired binding revived: %v", err)
			}
			if exact, err := LocateCodexCommandSession(ctx, cwd, liveID); err != nil || exact != live {
				t.Fatalf("exact owner=%q error=%v", exact, err)
			}
			if registered, err := LocateRegisteredAppSession(cwd, domain.ProviderCodex, liveID); err != nil || registered != live {
				t.Fatalf("registered owner=%q error=%v", registered, err)
			}
			if sessions := ActiveAppSessions(cwd); len(sessions) != 1 || sessions[0].SessionID != liveID || sessions[0].Path != live {
				t.Fatalf("app liveness=%+v", sessions)
			}
			for path, raw := range before {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, raw) {
					t.Fatalf("discovery changed synthetic archive %q: %v", path, err)
				}
			}
		})
	}
}
