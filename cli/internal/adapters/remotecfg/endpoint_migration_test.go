package remotecfg

import (
	"context"
	"encoding/json"
	"testing"
)

func TestEndpointPendingReceiptMatchesOnlyExactTransition(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := configFixtureSave(root, Remotes{"origin": migrationIdentity}); err != nil {
		t.Fatal(err)
	}
	before := observeConfig(t, root)
	if err := SetEndpointWithPending(ctx, before, "origin", migrationBinding(), false, true); err != nil {
		t.Fatal(err)
	}
	current := observeConfig(t, root)
	sources, err := current.PendingEndpointSources(migrationIdentity, RepoIDFor(migrationIdentity))
	if err != nil || len(sources) != 1 || sources[0].Version != 0 || sources[0].Endpoint != "http://localhost:5173/api/v1" {
		t.Fatal(sources, err)
	}
	if wrong, err := current.PendingEndpointSources(migrationIdentity, RepoIDFor("https://other.test/acme/project")); err != nil || len(wrong) != 0 {
		t.Fatal("receipt crossed repository", err)
	}
	if err := SetEndpointWithPending(ctx, current, "origin", migrationBinding(), false, true); err != nil {
		t.Fatal("idempotent reauthorization failed", err)
	}
	current = observeConfig(t, root)
	later := migrationBinding()
	later.Endpoint = "https://later.test/api/v1"
	if err := SetEndpoint(ctx, current, "origin", later, false); err != nil {
		t.Fatal(err)
	}
	if sources, err := observeConfig(t, root).PendingEndpointSources(migrationIdentity, later.RepoID); err != nil || len(sources) != 0 {
		t.Fatal("new endpoint piggybacked on old receipt", sources, err)
	}
	if err := SetEndpointWithPending(ctx, before, "origin", later, false, true); err == nil {
		t.Fatal("stale CAS published receipt")
	}
}

func TestEndpointRawBindingDoesNotAuthorizePending(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := configFixtureSave(root, Remotes{"origin": migrationIdentity}); err != nil {
		t.Fatal(err)
	}
	if err := setField(ctx, root, "transport_endpoints", map[string]EndpointBinding{migrationIdentity: migrationBinding()}); err != nil {
		t.Fatal(err)
	}
	current := observeConfig(t, root)
	if sources, err := current.PendingEndpointSources(migrationIdentity, RepoIDFor(migrationIdentity)); err != nil || len(sources) != 0 {
		t.Fatal("raw binding authorized pending", err)
	}
	if err := SetEndpointWithPending(ctx, current, "origin", migrationBinding(), false, true); err == nil {
		t.Fatal("same-endpoint call invented an unknown prior transition")
	}
	fields := readConfigFields(t, root)
	fields["transport_migrations"] = json.RawMessage(`{"forged":{"version":1}}`)
	if err := setField(ctx, root, "transport_migrations", fields["transport_migrations"]); err != nil {
		t.Fatal(err)
	}
	if _, err := observeConfig(t, root).PendingEndpointSources(migrationIdentity, RepoIDFor(migrationIdentity)); err == nil {
		t.Fatal("malformed receipt accepted")
	}
}
