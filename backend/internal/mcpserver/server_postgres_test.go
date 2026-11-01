//go:build postgres

package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	delivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/http"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

// The browser test exercises real binary OAuth/PKCE consent through Vite.
// This test isolates independent pools, restart continuity, route separation,
// membership changes and cross-service revocation against real PostgreSQL.
func TestPGIndependentMCP(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("requires a disposable CXT_TEST_DSN")
	}
	t.Setenv("CXT_POSTGRES_DSN", dsn)
	t.Setenv("CXT_AUTH", "dev")
	t.Setenv("CXT_PUBLIC_URL", "https://cxthub.example")
	dir, err := filepath.Abs("../../../schemas/db/migrations")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CXT_MIGRATIONS_DIR", dir)
	ctx := context.Background()
	setup, cancel := context.WithTimeout(ctx, 30*time.Second)
	seed, err := store.NewPostgresStore(setup, dsn)
	if err != nil {
		cancel()
		t.Fatal("open disposable MCP fixture seed store", err)
	}
	_, err = seed.ApplyPublicMigrations(setup, dir)
	seed.Close()
	cancel()
	if err != nil {
		t.Fatal("migrate the disposable MCP fixture database", err)
	}
	open := func() *serverruntime.Runtime {
		r, err := serverruntime.Open(ctx, "127.0.0.1:0", "", true)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	openMCP := func() *serverruntime.Runtime {
		runtime, err := serverruntime.OpenMCP(ctx, "127.0.0.1:0", "", true)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.Context != nil || runtime.ContextBranches != nil {
			t.Fatal("MCP constructed a command service")
		}
		return runtime
	}
	api, mcp := open(), openMCP()
	defer func() { api.Close(); mcp.Close() }()
	branches, actualOwner := api.ContextBranches.(*app.ContextBranchOperations)
	if !actualOwner || branches == nil {
		t.Fatalf("API root did not construct the actual context branch owner: %T", api.ContextBranches)
	}
	owner, _, err := api.Identity.Login(ctx, "dev:"+domain.NewID("owner")+"@example.test", "owner")
	if err != nil {
		t.Fatal(err)
	}
	actor, _, err := api.Identity.Login(ctx, "dev:"+domain.NewID("reader")+"@example.test", "reader")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := api.Identity.CreateRepository(ctx, owner, "independent-mcp")
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: domain.HashContent([]byte(repository.ID)), RepositoryID: repository.ID, RemoteURL: "https://cxthub.example/" + owner.Username + "/independent-mcp", DefaultBranch: "main"}
	if _, err = api.Store.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	document := domain.SessionDoc{CIR: domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex},
		Events:   []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "independent context query " + string(repo.ID)}}}},
	}}
	raw, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		t.Fatal(err)
	}
	document.Hash = domain.HashContent(raw)
	if _, err := api.Store.PutDoc(ctx, repo.ID, document); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.PutSnapshot(ctx, domain.Snapshot{ID: document.Hash, RepoID: repo.ID, DocHash: document.Hash, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.CompareAndSwapRef(ctx, repo.ID, domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", Target: document.Hash}, ""); err != nil {
		t.Fatal(err)
	}
	fork, err := branches.Fork(inbound.WithRepositoryActor(ctx, owner.ID), inbound.ForkInput{RepoID: repo.ID, FromSnapshot: document.Hash, NewBranch: "api-root-branch"})
	if err != nil || fork.Branch != "api-root-branch" || fork.Head != document.Hash {
		t.Fatal("actual API branch owner rejected the root's repository ports", fork, err)
	}
	if created, err := api.Store.GetRef(ctx, repo.ID, domain.RefBranch, fork.Branch); err != nil || created.Target != document.Hash {
		t.Fatal("actual API branch owner did not publish its source-bound ref", created, err)
	}
	if err = api.Store.AddMember(ctx, domain.Membership{RepositoryID: repository.ID, UserID: actor.ID, Role: domain.RolePuller, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	client := domain.NewID("mcp_client_")
	pair, err := mcp.Identity.IssueMCPTokenPair(ctx, actor.ID, client, mcp.PublicURL+"/mcp")
	if err != nil {
		t.Fatal(err)
	}
	makeMCP := func() *httptest.Server {
		h, err := Handler(mcp, mcp.PublicURL)
		if err != nil {
			t.Fatal(err)
		}
		return httptest.NewServer(h)
	}
	mcpServer := makeMCP()
	defer func() { mcpServer.Close() }()
	call := func(method, target, body, token string) (int, string) {
		req, err := http.NewRequest(method, target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(raw)
	}
	apiServer := httptest.NewServer(api.HealthHandler(delivery.NewServer(api.Context, api.Identity).Handler()))
	defer apiServer.Close()
	for _, target := range []string{"/mcp", "/oauth/register", "/.well-known/oauth-authorization-server", "/api/v1/oauth/requests/missing"} {
		if status, body := call("GET", apiServer.URL+target, "", ""); status != 404 {
			t.Fatalf("API exposes MCP route %s: %d %s", target, status, body)
		}
	}
	if status, body := call("POST", mcpServer.URL+"/api/v1/repositories", `{}`, pair.AccessToken); status != 404 {
		t.Fatalf("MCP exposes repository writes: %d %s", status, body)
	}
	listing := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"repository_list","arguments":{}}}`
	assertVisible := func(want bool) {
		status, body := call("POST", mcpServer.URL+"/mcp", listing, pair.AccessToken)
		if status != 200 || strings.Contains(body, `"isError":true`) || strings.Contains(body, string(repo.ID)) != want {
			t.Fatalf("visible=%v: %d %s", want, status, body)
		}
	}
	assertContextRead := func(want bool) {
		for _, tool := range []string{"context_list", "context_history", "context_fetch", "context_search", "git_changes", "git_observations"} {
			body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 42, "method": "tools/call", "params": map[string]any{
				"name": tool, "arguments": map[string]any{"repository": string(repo.ID), "branch": "main", "ref": string(document.Hash), "query": "independent context"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			status, response := call("POST", mcpServer.URL+"/mcp", string(body), pair.AccessToken)
			failed := strings.Contains(response, `"isError":true`)
			if status != 200 || failed == want || (want && tool == "context_list" && !strings.Contains(response, string(document.Hash))) {
				t.Fatalf("context=%s allowed=%v: %d %s", tool, want, status, response)
			}
		}
	}
	assertVisible(true)
	assertContextRead(true)
	// Closing the entire API pool/server must not interrupt MCP reads.
	apiServer.Close()
	api.Close()
	assertVisible(true)
	assertContextRead(true)
	api = open()
	// Restart MCP with a new pool; the existing token remains usable.
	mcpServer.Close()
	mcp.Close()
	mcp = openMCP()
	mcpServer = makeMCP()
	assertVisible(true)
	assertContextRead(true)
	if err := api.Identity.RemoveMember(ctx, owner.ID, repository.ID, actor.ID); err != nil {
		t.Fatal(err)
	}
	assertVisible(false)
	assertContextRead(false)
	if err := api.Identity.RevokeMCPApplication(ctx, actor.ID, client); err != nil {
		t.Fatal(err)
	}
	if status, body := call("POST", mcpServer.URL+"/mcp", listing, pair.AccessToken); status != 401 {
		t.Fatalf("revoked access survives: %d %s", status, body)
	}
	if _, err := mcp.Identity.RefreshMCPAccessToken(ctx, pair.RefreshToken, client); err == nil {
		t.Fatal("revoked refresh survives")
	}
}
