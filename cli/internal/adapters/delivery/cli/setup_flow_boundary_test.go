package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// A warning inherited from registration-only setup must not authorize the new
// automatic attachment from a repository other than the one explicitly asked for.
func TestSetupRequestedConnectionAuthority(t *testing.T) {
	for _, sameIdentity := range []bool{false, true} {
		name := "different-repository-must-stop"
		if sameIdentity {
			name = "same-identity-display-alias-is-valid"
		}
		t.Run(name, func(t *testing.T) {
			f := setupTrackingFixture(t)
			existing, _ := remotecfg.Origin(f.cwd)
			requested := "https://example.invalid/team/requested-context"
			stable := requested
			if sameIdentity {
				stable = existing
			}
			f.c.ResolveConnection = func(_ context.Context, raw string) (domain.RepositoryConnection, error) {
				if raw != requested {
					t.Fatalf("resolved unexpected URL %q", raw)
				}
				return domain.RepositoryConnection{RepoID: remotecfg.RepoIDFor(stable), RemoteURL: stable, RepositoryID: "synthetic-owner-record"}, nil
			}
			err := runSetup(context.Background(), f.c, f.cwd, []string{requested, "--no-login"})
			if sameIdentity {
				if err != nil || f.remote.pulls != 1 {
					t.Fatalf("valid alias failed: pulls=%d err=%v", f.remote.pulls, err)
				}
				return
			}
			p, perr := f.store.GetWorkingPosition(context.Background())
			_, hookErr := os.Stat(filepath.Join(f.cwd, ".claude", "settings.json"))
			if err == nil || f.remote.pulls != 0 || p.Snapshot != "" || !os.IsNotExist(hookErr) {
				t.Fatalf("requested a different repository but continued: err=%v fetches=%d positionRepo=%s selected=%s positionErr=%v hookErr=%v", err, f.remote.pulls, p.RepoID, p.Snapshot, perr, hookErr)
			}
		})
	}
}

func TestSetupGitEvidenceFailureIsNotDetached(t *testing.T) {
	for _, kind := range []string{"detached", "cancelled", "fatal-git-error"} {
		t.Run(kind, func(t *testing.T) {
			f := setupTrackingFixture(t)
			ctx := context.Background()
			switch kind {
			case "detached":
				runLifecycleGit(t, f.cwd, "checkout", "--detach", f.oid)
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			case "fatal-git-error":
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				if err = os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nif [ \"$3\" = symbolic-ref ]; then exit 128; fi\nexec \""+realGit+"\" \"$@\"\n"), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			got, err := readSetupGitTracking(ctx, f.cwd)
			if kind == "detached" {
				if err != nil || got.remote != "" {
					t.Fatalf("detached should be registration-only: %+v %v", got, err)
				}
			} else if err == nil {
				t.Fatalf("%s silently classified as detached: %+v", kind, got)
			}
		})
	}
}
