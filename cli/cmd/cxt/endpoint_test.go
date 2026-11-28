package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/authcfg"
	delivcli "github.com/wnsdy95/cxthub-cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

const endpointTestIdentity = "http://localhost:5173/acme/project"
const endpointTestDestination = "https://cxthub.io/api/v1"

type endpointFixture struct {
	cfg           config
	requests      int
	registrations int
	mode          string
	afterRead     func()
}

func newEndpointFixture(t *testing.T) *endpointFixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	mainTestGit(t, root, "init", "--template=", "-q", "-b", "main")
	root, _ = filepath.EvalSymlinks(root)
	t.Chdir(root)
	if err := configFixtureSave(root, remotecfg.Remotes{"origin": endpointTestIdentity, "mirror": endpointTestIdentity}); err != nil {
		t.Fatal(err)
	}
	fixture := &endpointFixture{cfg: config{RepoRoot: root, GitDir: filepath.Join(root, ".git"), GitBranch: "main", RemoteEndpoint: "http://localhost:5173/api/v1", RemoteToken: "old-host-synthetic-secret"}}
	if err := authcfg.Save("cxthub.io", "destination-synthetic-secret"); err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = capturePushRoundTrip(func(request *http.Request) (*http.Response, error) {
		fixture.requests++
		if request.URL.Scheme != "https" || request.URL.Host != "cxthub.io" || request.Header.Get("Authorization") != "Bearer destination-synthetic-secret" {
			t.Fatal("request escaped the verified destination or forwarded an old credential")
		}
		repoID := remotecfg.RepoIDFor(endpointTestIdentity)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/me":
			if fixture.mode == "unauthorized" {
				return capturePushJSON(t, request, 401, map[string]string{"error": "synthetic denial"}), nil
			}
			if fixture.mode == "redirect" {
				response := capturePushJSON(t, request, 307, nil)
				response.Header.Set("Location", "https://sub.cxthub.io/api/v1/me")
				return response, nil
			}
			return capturePushJSON(t, request, 200, map[string]string{"id": "destination-account"}), nil
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/repository-connections":
			if request.URL.Query().Get("remote_url") != endpointTestIdentity {
				t.Fatal("migration queried a new URL-derived identity")
			}
			connection := domain.RepositoryConnection{RepoID: repoID, RemoteURL: endpointTestIdentity, RepositoryID: "repository-fixture"}
			switch fixture.mode {
			case "wrong-url":
				connection.RemoteURL = "https://cxthub.io/acme/project"
			case "wrong-repo":
				connection.RepoID = remotecfg.RepoIDFor("https://cxthub.io/acme/project")
			case "missing-owner":
				connection.RepositoryID = ""
			case "changed-owner":
				connection.RepositoryID = "other-repository"
			}
			return capturePushJSON(t, request, 200, connection), nil
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/repos/"+repoID:
			if fixture.afterRead != nil {
				after := fixture.afterRead
				fixture.afterRead = nil
				after()
			}
			repo := map[string]any{"id": repoID, "remote_url": endpointTestIdentity, "repository_id": "repository-fixture", "default_branch": "main", "context_protocol": 1, "required_doc_identity": domain.DocumentIdentityRootV1, "root_publication_enabled": fixture.mode != "no-root", "doc_identities_supported": []domain.DocumentIdentity{domain.DocumentIdentityRootV1}}
			switch fixture.mode {
			case "different-owner", "changed-owner":
				repo["repository_id"] = "other-repository"
			case "different-row-url":
				repo["remote_url"] = "https://cxthub.io/acme/project"
			case "different-row-id":
				repo["id"] = remotecfg.RepoIDFor("https://cxthub.io/acme/project")
			case "missing-row":
				return capturePushJSON(t, request, 404, nil), nil
			}
			return capturePushJSON(t, request, 200, repo), nil
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/push/negotiate"):
			return capturePushJSON(t, request, 200, map[string]any{"root_publication_enabled": true, "doc_identities_supported": []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, "async_docs_supported": true, "chunks_supported": true, "bounded_chunks_supported": true, "chunk_formats_supported": []string{domain.ConversationManifestChunkFormat}}), nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/repos":
			fixture.registrations++
			return capturePushJSON(t, request, 200, domain.Repo{ID: repoID, RemoteURL: endpointTestIdentity, RepositoryID: "repository-fixture", DefaultBranch: "main", ContextProtocol: 1}), nil
		default:
			if fixture.mode == "repair-denied" {
				return capturePushJSON(t, request, 403, map[string]string{"error": "synthetic repair denial"}), nil
			}
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
			return nil, errors.New("unexpected request")
		}
	})
	return fixture
}

func (fixture *endpointFixture) migrate(t *testing.T) error {
	t.Helper()
	args := []string{"cxt", "remote", "set-endpoint", "origin", endpointTestDestination}
	return delivcli.Run(&delivcli.Container{SetRemoteEndpoint: setRemoteEndpoint(fixture.cfg)}, args)
}

func TestEndpointMigrationInsecureInputAndPassiveResolution(t *testing.T) {
	fixture := newEndpointFixture(t)
	ctx := context.Background()
	observed, err := remotecfg.Observe(ctx, fixture.cfg.RepoRoot)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(fixture.cfg.RepoRoot, ".cxt/config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"http://cxthub.io/api/v1", "https://credential@cxthub.io/api/v1", "https://cxthub.io/api/v1?credential=private"} {
		if err := setRemoteEndpoint(fixture.cfg)(ctx, observed, "origin", endpoint); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("unsafe endpoint accepted or leaked", err)
		}
	}
	if err := fixture.migrate(t); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(fixture.cfg.RepoRoot, ".cxt/config"))
	if err != nil || bytes.Equal(before, current) {
		t.Fatal("migration was not persisted", err)
	}
	calls := fixture.requests
	for attempt := 0; attempt < 3; attempt++ {
		endpoint, token, err := configuredTransport(fixture.cfg)
		if err != nil || endpoint != endpointTestDestination || token != "destination-synthetic-secret" {
			t.Fatal("passive resolution failed")
		}
	}
	after, err := os.ReadFile(filepath.Join(fixture.cfg.RepoRoot, ".cxt/config"))
	if err != nil || !bytes.Equal(current, after) || fixture.requests != calls {
		t.Fatal("passive resolution had side effects", err)
	}
}
