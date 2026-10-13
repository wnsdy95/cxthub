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
	"github.com/wnsdy95/cxthub-cli/internal/adapters/branchjournal"
	delivcli "github.com/wnsdy95/cxthub-cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
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

func TestEndpointRepairUsesVerifiedBindingAndPreservesIdentity(t *testing.T) {
	fixture := newEndpointFixture(t)
	ctx := context.Background()
	if err := fixture.migrate(t); err != nil {
		t.Fatal(err)
	}
	journal, err := branchjournal.Open(ctx, fixture.cfg.RepoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Bind(remotecfg.RepoIDFor(endpointTestIdentity)); err != nil {
		t.Fatal(err)
	}
	fixture.mode = "repair-denied"
	t.Setenv("CXT_TOKEN", "old-host-synthetic-secret")
	before, err := os.ReadFile(filepath.Join(fixture.cfg.RepoRoot, ".cxt/config"))
	if err != nil {
		t.Fatal(err)
	}
	calls := fixture.requests
	if err := runRepair([]string{"--from-server"}); err == nil || fixture.requests < calls+3 {
		t.Fatal("repair did not verify identity and use migrated transport", err)
	}
	after, err := os.ReadFile(filepath.Join(fixture.cfg.RepoRoot, ".cxt/config"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("repair changed bound configuration", err)
	}
}

func TestEndpointRepairRejectsDamagedConfigBeforeNetwork(t *testing.T) {
	fixture := newEndpointFixture(t)
	if err := fixture.migrate(t); err != nil {
		t.Fatal(err)
	}
	journal, err := branchjournal.Open(context.Background(), fixture.cfg.RepoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Bind(remotecfg.RepoIDFor(endpointTestIdentity)); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(fixture.cfg.RepoRoot, ".cxt/config")
	damaged := []byte("broken config fixture\n")
	if err := os.WriteFile(configPath, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	calls := fixture.requests
	if err := runRepair([]string{"--from-server", "--remote", endpointTestIdentity}); err == nil || fixture.requests != calls {
		t.Fatal("repair guessed a destination from damaged configuration", err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(after, damaged) {
		t.Fatal("rejected repair changed the damaged configuration", err)
	}
}

func TestEndpointFrozenCaptureRejectsWrongBinding(t *testing.T) {
	fixture := newEndpointFixture(t)
	ctx := context.Background()
	store, _, _ := buildRepositoryAdapters(fixture.cfg)
	for _, mode := range []string{"different-owner", "wrong-url", "wrong-repo", "missing-row"} {
		fixture.mode = mode
		destination := delivcli.CapturePushDestination{URL: endpointTestIdentity, Endpoint: endpointTestDestination, RepositoryID: "repository-fixture"}
		_, err := prepareCapturePush(fixture.cfg, store)(ctx, fixture.cfg.RepoRoot, destination, remotecfg.RepoIDFor(endpointTestIdentity))
		if err == nil {
			t.Fatal("frozen migration accepted wrong identity", mode)
		}
		if mode != "missing-row" && !errors.Is(err, remotecfg.ErrEndpointBindingMismatch) {
			t.Fatal("identity mismatch cannot preserve retry budget", err)
		}
	}
	if fixture.registrations != 0 {
		t.Fatal("rejected capture registered repository")
	}
}

func (fixture *endpointFixture) migrate(t *testing.T, pending ...bool) error {
	t.Helper()
	args := []string{"cxt", "remote", "set-endpoint", "origin", endpointTestDestination}
	if len(pending) != 0 && pending[0] {
		args = append(args, "--migrate-pending")
	}
	return delivcli.Run(&delivcli.Container{SetRemoteEndpoint: setRemoteEndpoint(fixture.cfg)}, args)
}

func TestEndpointPendingReceiptRequiresVerifiedCAS(t *testing.T) {
	for _, mode := range []string{"success", "wrong-repo", "config-race"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newEndpointFixture(t)
			fixture.mode = mode
			if mode == "config-race" {
				fixture.afterRead = func() {
					if err := remotecfg.SetLoadMode(context.Background(), fixture.cfg.RepoRoot, "memory"); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := fixture.migrate(t, true)
			observed, readErr := remotecfg.Observe(context.Background(), fixture.cfg.RepoRoot)
			if readErr != nil {
				t.Fatal(readErr)
			}
			sources, readErr := observed.PendingEndpointSources(endpointTestIdentity, remotecfg.RepoIDFor(endpointTestIdentity))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "success" {
				if err != nil || len(sources) != 1 || sources[0].Version != 0 || sources[0].Endpoint != "http://localhost:5173/api/v1" || sources[0].RemoteURL != endpointTestIdentity {
					t.Fatal("verified receipt missing", sources, err)
				}
			} else if err == nil || len(sources) != 0 {
				t.Fatal("failed migration authorized pending work", err)
			}
		})
	}
}

func TestEndpointMigrationSuccessfulMoveAllTransports(t *testing.T) {
	fixture := newEndpointFixture(t)
	ctx := context.Background()
	root := fixture.cfg.RepoRoot
	observed, err := remotecfg.Observe(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := remotecfg.SetCaptureDocumentIdentity(ctx, observed, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	store, normal, git := buildRepositoryAdapters(fixture.cfg)
	if fixture.requests != 0 {
		t.Fatal("adapter construction performed network I/O")
	}
	oldCache := normal.SyncRemoteIdentity()
	if err := fixture.migrate(t); err != nil {
		t.Fatal(err)
	}
	if fixture.requests != 5 || fixture.registrations != 0 {
		t.Fatal("migration wrote to server or missed verification", fixture.requests)
	}
	if oldCache == normal.SyncRemoteIdentity() || normal.SyncRemoteIdentity() != endpointTestDestination {
		t.Fatal("cache endpoint scope unchanged")
	}
	repo, err := git.CurrentRepo(ctx, root)
	if err != nil || repo.ID != remotecfg.RepoIDFor(endpointTestIdentity) || repo.RemoteURL != endpointTestIdentity {
		t.Fatal(repo, err)
	}
	if _, err := normal.Repository(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if identity, err := remotecfg.CaptureIdentity(ctx, root); err != nil || identity != domain.DocumentIdentityRootV1 {
		t.Fatal(identity, err)
	}
	if _, err := namedSyncDestination(fixture.cfg, store, git)(ctx, root, "mirror"); err != nil {
		t.Fatal(err)
	}
	container := buildContainer(fixture.cfg)
	connection, err := container.clictr.ResolveConnection(ctx, endpointTestIdentity)
	if err != nil || connection.RepoID != repo.ID || connection.RepositoryID != "repository-fixture" {
		t.Fatal(connection, err)
	}
	if err := setCaptureIdentity(fixture.cfg)(ctx, root, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	destination := delivcli.CapturePushDestination{URL: endpointTestIdentity, Endpoint: endpointTestDestination, RepositoryID: "repository-fixture"}
	syncer, err := prepareCapturePush(fixture.cfg, store)(ctx, root, destination, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	observed, err = remotecfg.Observe(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := observed.ResolveEndpoint(endpointTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	binding.Endpoint = "https://unrelated.example/api/v1"
	if err := remotecfg.SetEndpoint(ctx, observed, "origin", binding, true); err != nil {
		t.Fatal(err)
	}
	if err := authcfg.Save("cxthub.io", "rotated-after-preparation"); err != nil {
		t.Fatal(err)
	}
	out, err := syncer.Connect(ctx, inbound.SyncInput{Cwd: root})
	if err != nil || out.Repo.ID != repo.ID || out.Repo.RemoteURL != endpointTestIdentity {
		t.Fatal(out, err)
	}
}

func TestEndpointMigrationRejectsUnverifiedMovesWithoutConfigChanges(t *testing.T) {
	for _, mode := range []string{"unauthorized", "redirect", "wrong-url", "wrong-repo", "missing-owner", "different-owner", "different-row-url", "different-row-id", "missing-row", "no-root", "missing-login", "changed-owner", "config-race", "origin-race"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newEndpointFixture(t)
			ctx := context.Background()
			root := fixture.cfg.RepoRoot
			if mode == "changed-owner" {
				if err := fixture.migrate(t); err != nil {
					t.Fatal(err)
				}
			}
			fixture.mode = mode
			if mode == "missing-login" {
				if err := authcfg.Delete("cxthub.io"); err != nil {
					t.Fatal(err)
				}
			}
			observed, err := remotecfg.Observe(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "no-root" {
				if err := remotecfg.SetCaptureDocumentIdentity(ctx, observed, domain.DocumentIdentityRootV1); err != nil {
					t.Fatal(err)
				}
			}
			configPath := filepath.Join(root, ".cxt/config")
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "config-race" || mode == "origin-race" {
				fixture.afterRead = func() {
					if mode == "config-race" {
						if err := remotecfg.SetLoadMode(ctx, root, "memory"); err != nil {
							t.Fatal(err)
						}
					} else if err := configFixtureSave(root, remotecfg.Remotes{"origin": "https://other.test/team/repo"}); err != nil {
						t.Fatal(err)
					}
					before, err = os.ReadFile(configPath)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			err = fixture.migrate(t)
			if err == nil {
				t.Fatal("unverified migration succeeded")
			}
			if strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("secret in error")
			}
			if (mode == "config-race" || mode == "origin-race") && !errors.Is(err, remotecfg.ErrChanged) {
				t.Fatal("not a CAS failure", err)
			}
			after, readErr := os.ReadFile(configPath)
			if readErr != nil || !bytes.Equal(before, after) || fixture.registrations != 0 {
				t.Fatal("failed migration changed config or server", readErr)
			}
			if mode == "missing-login" && fixture.requests != 0 {
				t.Fatal("old token used for migration")
			}
		})
	}
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
		if err := setRemoteEndpoint(fixture.cfg)(ctx, observed, "origin", endpoint, false); err == nil || strings.Contains(err.Error(), "private") {
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
