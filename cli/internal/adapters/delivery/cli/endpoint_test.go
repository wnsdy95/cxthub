package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
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
	if err := testDeviceLoginContainer().loginWithToken(context.Background(), server.URL+"/api/v1", parsed.Host, "synthetic-login"); err == nil {
		t.Fatal("redirected login succeeded")
	}
	if authcfg.Token(parsed.Host) != "" {
		t.Fatal("failed login saved credential")
	}
}

func TestEndpointCommandArgumentContract(t *testing.T) {
	valid := []string{"cxt", "remote", "set-endpoint", "origin", "https://cxthub.io/api/v1"}
	if _, err := PreflightArgs(valid); err != nil {
		t.Fatal("a valid endpoint command was rejected", err)
	}
	for _, args := range [][]string{{"cxt", "remote", "set-endpoint"}, {"cxt", "remote", "set-endpoint", "origin"}, {"cxt", "remote", "set-endpoint", "origin", "https://cxthub.io/api/v1", "extra"}, {"cxt", "remote", "set-endpoint", "origin", "https://cxthub.io/api/v1", "-v"}} {
		if _, err := PreflightArgs(args); err == nil {
			t.Fatal("invalid migration syntax accepted")
		}
	}
}
