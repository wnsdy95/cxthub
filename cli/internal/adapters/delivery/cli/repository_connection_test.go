package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestRemoteResolvesDisplayAddressBeforePersistingStableIdentity(t *testing.T) {
	const display = "https://cxthub.example/acme/backend"
	const original = "https://cxthub.example/acme/platform/backend"
	for _, scenario := range []string{"success", "offline", "bad-hash", "other-host", "missing-boundary"} {
		t.Run(scenario, func(t *testing.T) {
			cwd := t.TempDir()
			c := &Container{ResolveConnection: func(_ context.Context, requested string) (domain.RepositoryConnection, error) {
				if requested != display {
					t.Fatalf("requested %s", requested)
				}
				result := domain.RepositoryConnection{RepositoryID: "ws_0123456789abcdef0123456789abcdef", RepoID: remotecfg.RepoIDFor(original), RemoteURL: original}
				switch scenario {
				case "offline":
					return result, errors.New("server unavailable")
				case "bad-hash":
					result.RepoID = remotecfg.RepoIDFor(display)
				case "other-host":
					result.RemoteURL = "https://other.example/acme/backend"
					result.RepoID = remotecfg.RepoIDFor(result.RemoteURL)
				case "missing-boundary":
					result.RepositoryID = ""
				}
				return result, nil
			}}
			err := runRemote(context.Background(), c, cwd, []string{"add", "review", display})
			remotes, readErr := remotecfg.Load(cwd)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if scenario == "success" {
				if err != nil || remotes["review"] != original {
					t.Fatalf("stable remote = %v, error %v", remotes, err)
				}
			} else if err == nil || len(remotes) != 0 {
				t.Fatalf("unverified connection persisted: %v, error %v", remotes, err)
			}
		})
	}
}

func TestLoginTargetBeforeConnection(t *testing.T) {
	cwd := t.TempDir()
	base, host, err := loginTarget(cwd, "https://cxthub.example/")
	if err != nil || base != "https://cxthub.example/api/v1" || host != "cxthub.example" {
		t.Fatalf("login target: %s %s %v", base, host, err)
	}
	for _, invalid := range []string{"//example.test", "file:///tmp/server", "https://token@example.test", "https://example.test/user/repo", "https://example.test/?key=secret"} {
		if _, _, err = loginTarget(cwd, invalid); err == nil {
			t.Fatalf("accepted ambiguous target %s", invalid)
		}
	}
	if _, err := PreflightArgs([]string{"cxt", "login", "--server", "https://cxthub.example"}); err != nil {
		t.Fatal(err)
	}
	if got := firstPositional([]string{"--server", "https://cxthub.example"}); got != "" {
		t.Fatalf("server URL treated as token: %s", got)
	}
}

func TestSetupRecognizesMigratedDisplayAddress(t *testing.T) {
	const original = "https://cxthub.example/acme/platform/backend"
	const display = "https://cxthub.example/acme/backend"
	calls := 0
	c := &Container{ResolveConnection: func(_ context.Context, address string) (domain.RepositoryConnection, error) {
		calls++
		if address != display {
			t.Fatalf("resolving unexpected address %s", address)
		}
		return domain.RepositoryConnection{RepositoryID: "ws_0123456789abcdef0123456789abcdef", RemoteURL: original, RepoID: remotecfg.RepoIDFor(original)}, nil
	}}
	if same, err := setupRemoteMatches(context.Background(), c, original, display); err != nil || !same || calls != 1 {
		t.Fatalf("migrated connection same=%v calls=%d err=%v", same, calls, err)
	}
	if same, err := setupRemoteMatches(context.Background(), c, original, original); err != nil || !same || calls != 1 {
		t.Fatalf("unchanged connection should not need the server: same=%v calls=%d err=%v", same, calls, err)
	}
	if same, err := setupRemoteMatches(context.Background(), c, "https://cxthub.example/acme/other", display); err != nil || same {
		t.Fatalf("distinct repository matched: same=%v err=%v", same, err)
	}
	c.ResolveConnection = func(context.Context, string) (domain.RepositoryConnection, error) {
		return domain.RepositoryConnection{}, errors.New("offline")
	}
	if same, err := setupRemoteMatches(context.Background(), c, original, display); err == nil || same {
		t.Fatalf("unverified alias matched: same=%v err=%v", same, err)
	}
}
