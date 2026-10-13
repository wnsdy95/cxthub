package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

func setTestEndpoint(t *testing.T, root, origin, endpoint string) {
	t.Helper()
	ctx := context.Background()
	observed, err := remotecfg.Observe(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	binding := remotecfg.EndpointBinding{Version: 1, RemoteURL: origin, RepoID: remotecfg.RepoIDFor(origin), RepositoryID: "repository-fixture", Endpoint: endpoint}
	if err := remotecfg.SetEndpoint(ctx, observed, "origin", binding, false); err != nil {
		t.Fatal(err)
	}
}

func TestEndpointLoginUsesMigrationDestination(t *testing.T) {
	root := t.TempDir()
	const origin = "http://localhost:5173/acme/project"
	writeCapturePushOrigin(t, root, origin)
	setTestEndpoint(t, root, origin, "https://cxthub.io/api/v1")
	base, host, err := loginTarget(root, "")
	if err != nil || base != "https://cxthub.io/api/v1" || host != "cxthub.io" {
		t.Fatal(base, host, err)
	}
	for _, invalid := range []string{"http://cxthub.io", "https://cxthub.io?", "https://cxthub.io#"} {
		if _, _, err := loginTarget(root, invalid); err == nil {
			t.Fatal("unsafe login target accepted")
		}
	}
}

func TestEndpointLoginDoesNotFollowCredentialRedirects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("login forwarded credentials to redirected destination")
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL+"/api/v1/me", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	if err := loginWithToken(context.Background(), server.URL+"/api/v1", parsed.Host, "synthetic-login"); err == nil {
		t.Fatal("redirected login succeeded")
	}
	if authcfg.Token(parsed.Host) != "" {
		t.Fatal("failed login saved credential")
	}
}

func TestEndpointPersonalPreferencesIsolateCredentialsAndCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CXT_TOKEN", "old-host-synthetic-secret")
	root := t.TempDir()
	const origin = "http://localhost:5173/acme/project"
	writeCapturePushOrigin(t, root, origin)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Header.Get("Authorization") != "Bearer destination-synthetic-secret" {
			t.Error("old token leaked through preferences")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"load_mode":"memory"}`))
	}))
	defer server.Close()
	setTestEndpoint(t, root, origin, server.URL+"/api/v1")
	if mode := serverLoadMode(root); mode != "" || calls.Load() != 0 {
		t.Fatal("missing destination login used old token")
	}
	parsed, _ := url.Parse(server.URL)
	if err := authcfg.Save(parsed.Host, "destination-synthetic-secret"); err != nil {
		t.Fatal(err)
	}
	if mode := serverLoadMode(root); mode != "memory" || calls.Load() != 1 {
		t.Fatal("destination preference not loaded")
	}
	other := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writer.WriteHeader(503) }))
	defer other.Close()
	setTestEndpoint(t, root, origin, other.URL+"/api/v1")
	parsed, _ = url.Parse(other.URL)
	if err := authcfg.Save(parsed.Host, "other-destination-secret"); err != nil {
		t.Fatal(err)
	}
	if mode := serverLoadMode(root); mode != "" {
		t.Fatal("old endpoint cache crossed migration")
	}
}

func TestEndpointCommandArgumentContract(t *testing.T) {
	valid := []string{"cxt", "remote", "set-endpoint", "origin", "https://cxthub.io/api/v1"}
	if _, err := PreflightArgs(valid); err != nil || ReadOnlyInvocation(valid) {
		t.Fatal("migration is not an explicit mutating command", err)
	}
	for _, args := range [][]string{{"cxt", "remote", "set-endpoint"}, {"cxt", "remote", "set-endpoint", "origin"}, {"cxt", "remote", "set-endpoint", "origin", "https://cxthub.io/api/v1", "extra"}, {"cxt", "remote", "set-endpoint", "origin", "https://cxthub.io/api/v1", "-v"}} {
		if _, err := PreflightArgs(args); err == nil {
			t.Fatal("invalid migration syntax accepted")
		}
	}
}

func TestEndpointMigrationPreservesHistoricalCaptureQueue(t *testing.T) {
	fixture := newCapturePushDriver(t)
	ctx := context.Background()
	pass := fixture.attempt(t, "historical-transport", domain.DocumentIdentityLegacy, true)
	if err := processFrozenCapture(ctx, fixture.c, fixture.cwd, fixture.cwd, pass); err != nil {
		t.Fatal(err)
	}
	updates, scope := capturePushSelection(pass)
	if err := queueCapturePush(ctx, fixture.c, fixture.cwd, updates, scope); err != nil {
		t.Fatal(err)
	}
	_, before, raw := readCapturePushRequest(t, fixture.cwd)
	setTestEndpoint(t, fixture.cwd, capturePushTestContext, "https://migrated.test/api/v1")
	fixture.c.PrepareCapturePush = func(context.Context, string, CapturePushDestination, string) (inbound.SyncRepo, error) {
		t.Fatal("historical job silently retargeted")
		return nil, nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		if next, err := drainCapturePush(ctx, fixture.c, fixture.cwd, fixture.cwd, fixture.repo.ID); err != nil || !next.IsZero() {
			t.Fatal(next, err)
		}
	}
	_, after, retained := readCapturePushRequest(t, fixture.cwd)
	if !reflect.DeepEqual(before, after) || !bytes.Equal(raw, retained) {
		t.Fatal("migration consumed retry budget or rewrote historical intent")
	}
}

func TestEndpointCaptureRetryBindsTransportAndRepository(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(map[bool]string{false: "frozen-append", true: "endpoint-changed-during-append"}[change], func(t *testing.T) {
			fixture := newCapturePushDriver(t)
			ctx := context.Background()
			setTestEndpoint(t, fixture.cwd, capturePushTestContext, "https://migrated.test/api/v1")
			pass := fixture.attempt(t, "migrated-append", domain.DocumentIdentityLegacy, true)
			if err := processFrozenCapture(ctx, fixture.c, fixture.cwd, fixture.cwd, pass); err != nil {
				t.Fatal(err)
			}
			updates, scope := capturePushSelection(pass)
			if err := queueCapturePush(ctx, fixture.c, fixture.cwd, updates, scope); err != nil {
				t.Fatal(err)
			}
			_, queued, _ := readCapturePushRequest(t, fixture.cwd)
			if queued.Intent.ContextTransport == "" {
				t.Fatal("new job did not bind transport")
			}
			spy := capturePushSpy(t, fixture, pass)
			prepared := 0
			fixture.c.PrepareCapturePush = func(_ context.Context, cwd string, destination CapturePushDestination, repo string) (inbound.SyncRepo, error) {
				prepared++
				if prepared != 1 || repo != fixture.repo.ID || destination.URL != capturePushTestContext || destination.Endpoint != "https://migrated.test/api/v1" || destination.RepositoryID != "repository-fixture" {
					t.Fatal("factory lost immutable identity or transport")
				}
				return spy, nil
			}
			spy.before = func() {
				spy.failure = nil
				if len(spy.inputs) == 1 {
					spy.failure = domain.ErrSyncConflict
					if change {
						setTestEndpoint(t, fixture.cwd, capturePushTestContext, "https://later.test/api/v1")
					}
				}
			}
			if _, err := drainCapturePush(ctx, fixture.c, fixture.cwd, fixture.cwd, fixture.repo.ID); err != nil {
				t.Fatal(err)
			}
			_, after, _ := readCapturePushRequest(t, fixture.cwd)
			if after.Intent != queued.Intent || prepared != 1 {
				t.Fatal("retry changed durable binding")
			}
			if change {
				if after.Done || len(spy.inputs) != 1 {
					t.Fatal("append rerouted after endpoint drift")
				}
			} else if !after.Done || len(spy.inputs) != 2 || !spy.inputs[1].Append {
				t.Fatal("migrated append retry did not complete")
			}
		})
	}
}

func TestEndpointMergeDeliveryHashPreservesOrdinaryFormat(t *testing.T) {
	ordinary := CapturePushDestination{URL: capturePushTestContext}
	want := domain.HashContent([]byte(`{"URL":"` + capturePushTestContext + `","APIOnly":false}`))
	if mergeDestinationHash(ordinary) != want {
		t.Fatal("legacy merge delivery hash changed")
	}
	migrated := ordinary
	migrated.Endpoint, migrated.RepositoryID = "https://migrated.test/api/v1", "repository-fixture"
	if mergeDestinationHash(migrated) == want {
		t.Fatal("merge job omitted migrated transport binding")
	}
	wrong := migrated
	wrong.RepositoryID = "other-repository"
	if captureTransportHash(wrong) == captureTransportHash(migrated) {
		t.Fatal("capture binding omitted repository ownership identity")
	}
}
