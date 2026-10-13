package remotecfg

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

const migrationIdentity = "http://localhost:5173/acme/project"

func migrationBinding() EndpointBinding {
	return EndpointBinding{Version: 1, RemoteURL: migrationIdentity, RepoID: RepoIDFor(migrationIdentity), RepositoryID: "repository-fixture", Endpoint: "https://cxthub.io/api/v1"}
}

func TestEndpointValidation(t *testing.T) {
	for _, raw := range []string{"https://cxthub.io/api/v1", "https://CXTHUB.IO/api/v1/", "http://localhost:5173/api/v1", "http://127.0.0.1:8080/api/v1", "http://[::1]:8080/api/v1"} {
		if _, err := CanonicalEndpoint(raw); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"http://cxthub.io/api/v1", "http://localhost.evil/api/v1", "http://0.0.0.0/api/v1", "http://192.168.1.1/api/v1", "https://token@cxthub.io/api/v1", "https://cxthub.io/api/v1?secret=token", "https://cxthub.io/api/v1?", "https://cxthub.io/api/v1#", "https://cxthub.io/api/v1#fragment", "https://cxthub.io/api%2fv1", "https://cxthub.io/api/v1//", "https://cxthub.io/api/../api/v1", "https://cxthub.io:0/api/v1", "https://cxthub.io:65536/api/v1", "https://cxthub.io:/api/v1", "//cxthub.io/api/v1", "file:///api/v1", "https://cxthub.io/acme/project"} {
		if _, err := CanonicalEndpoint(raw); err == nil {
			t.Fatalf("accepted unsafe endpoint %q", raw)
		}
	}
}

func TestEndpointResolutionIsPassive(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	binding, err := ResolveEndpoint(ctx, root, migrationIdentity)
	if err != nil || binding.Endpoint != "http://localhost:5173/api/v1" || binding.Version != 0 || binding.RepoID != RepoIDFor(migrationIdentity) {
		t.Fatal(binding, err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatal("passive resolution created files")
	}
}

func TestEndpointCASPreservesIdentityAndCapture(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := configFixtureSave(root, Remotes{"origin": migrationIdentity, "mirror": migrationIdentity}); err != nil {
		t.Fatal(err)
	}
	if err := SetCaptureDocumentIdentity(ctx, observeConfig(t, root), domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	before := observeConfig(t, root)
	if err := SetEndpoint(ctx, before, "origin", migrationBinding(), false); err == nil || !sameObservation(before, observeConfig(t, root)) {
		t.Fatal("unconfirmed capture capability changed config", err)
	}
	if err := SetEndpoint(ctx, before, "origin", migrationBinding(), true); err != nil {
		t.Fatal(err)
	}
	current := observeConfig(t, root)
	binding, err := current.ResolveEndpoint(migrationIdentity)
	if err != nil || binding != migrationBinding() {
		t.Fatal(binding, err)
	}
	remotes, err := current.Remotes()
	if err != nil || remotes["origin"] != migrationIdentity || remotes["mirror"] != migrationIdentity {
		t.Fatal(remotes, err)
	}
	if identity, err := current.CaptureIdentity(); err != nil || identity != domain.DocumentIdentityRootV1 {
		t.Fatal(identity, err)
	}
	if err := SetEndpoint(ctx, before, "origin", migrationBinding(), true); !errors.Is(err, ErrChanged) || !sameObservation(current, observeConfig(t, root)) {
		t.Fatal("stale endpoint CAS changed config", err)
	}
	wrong := migrationBinding()
	wrong.RepositoryID = "different-owner"
	if err := SetEndpoint(ctx, current, "origin", wrong, true); err == nil || !sameObservation(current, observeConfig(t, root)) {
		t.Fatal("repository binding drift accepted", err)
	}
}

func TestEndpointLinkedWorktreeSharedCASAndGate(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root, linked := t.TempDir(), filepath.Join(t.TempDir(), "linked")
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	git("init", "--template=", "-q", "-b", "main")
	git("-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "fixture")
	git("worktree", "add", "-qb", "linked", linked)
	ctx := context.Background()
	if err := configFixtureSave(root, Remotes{"origin": migrationIdentity}); err != nil {
		t.Fatal(err)
	}
	observed := observeConfig(t, linked)
	if err := storage.NewFileStore(root).WithCaptureTrackingGate(ctx, func(held context.Context) error {
		if err := SetEndpoint(held, observed, "origin", migrationBinding(), false); !errors.Is(err, domain.ErrSyncConflict) {
			t.Fatal("nested gate upgrade accepted", err)
		}
		wait, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		if err := SetEndpoint(wait, observed, "origin", migrationBinding(), false); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("migration bypassed shared capture gate", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := SetEndpoint(ctx, observed, "origin", migrationBinding(), false); err != nil {
		t.Fatal(err)
	}
	if binding, err := ResolveEndpoint(ctx, root, migrationIdentity); err != nil || binding != migrationBinding() {
		t.Fatal(binding, err)
	}
	if _, err := os.Stat(filepath.Join(linked, ".cxt")); !os.IsNotExist(err) {
		t.Fatal("migration wrote linked worktree config", err)
	}
}

func TestEndpointMalformedBindingFailsClosed(t *testing.T) {
	for _, field := range []string{"endpoint", "repo", "url", "version", "owner"} {
		t.Run(field, func(t *testing.T) {
			root := t.TempDir()
			ctx := context.Background()
			binding := migrationBinding()
			switch field {
			case "endpoint":
				binding.Endpoint = "http://cxthub.io/api/v1"
			case "repo":
				binding.RepoID = RepoIDFor("https://other.test/team/repo")
			case "url":
				binding.RemoteURL = "https://other.test/team/repo"
			case "version":
				binding.Version = 2
			case "owner":
				binding.RepositoryID = ""
			}
			if err := setField(ctx, root, "transport_endpoints", map[string]EndpointBinding{migrationIdentity: binding}); err != nil {
				t.Fatal(err)
			}
			before := observeConfig(t, root)
			if _, err := ResolveEndpoint(ctx, root, migrationIdentity); err == nil || !sameObservation(before, observeConfig(t, root)) {
				t.Fatal("invalid binding did not fail passively", err)
			}
		})
	}
}
