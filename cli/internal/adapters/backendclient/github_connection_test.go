package backendclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// The client asks the repository's own route, hands on what the server said,
// and reads a server that has no such route as a server that does not say.
func TestReadGitHubConnection(t *testing.T) {
	repo := "sha256:" + strings.Repeat("c", 64)
	status, answer := http.StatusOK, ""
	method, path := "", ""
	asked := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		method, path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	defer server.Close()
	client := NewBackendClient(func() string { return server.URL + "/api/v1" }, func() string { return "synthetic-token" }, domain.TeamIdentity{})
	ctx := context.Background()

	answer = `{"state":"not_connected","connect_url":"https://example.test/connect/github"}`
	got, err := client.ReadGitHubConnection(ctx, repo)
	want := domain.GitHubRepositoryConnection{State: domain.GitHubRepositoryNotConnected, ConnectURL: "https://example.test/connect/github"}
	if err != nil || got != want || method != http.MethodGet || path != "/api/v1/repos/"+repo+"/github-connection" {
		t.Fatalf("not connected: %+v %v (%s %s)", got, err, method, path)
	}
	answer = `{"state":"connected","connect_url":"https://example.test/connect/github"}`
	if got, err = client.ReadGitHubConnection(ctx, repo); err != nil || got.State != domain.GitHubRepositoryConnected || got.ConnectURL != "https://example.test/connect/github" {
		t.Fatalf("connected: %+v %v", got, err)
	}
	answer = `{"state":"not_configured"}`
	if got, err = client.ReadGitHubConnection(ctx, repo); err != nil || got.State != "not_configured" || got.ConnectURL != "" {
		t.Fatalf("a server without the GitHub App: %+v %v", got, err)
	}

	// A server from before the question.
	status, answer = http.StatusNotFound, "404 page not found\n"
	if got, err = client.ReadGitHubConnection(ctx, repo); err != nil || got != (domain.GitHubRepositoryConnection{}) {
		t.Fatalf("a server without the route: %+v %v", got, err)
	}
	// A refusal is an error, and carries no state.
	status, answer = http.StatusForbidden, `{"error":{"code":"forbidden","message":"insufficient role permissions","details":{}}}`
	if got, err = client.ReadGitHubConnection(ctx, repo); err == nil || got != (domain.GitHubRepositoryConnection{}) {
		t.Fatalf("a refusal: %+v %v", got, err)
	}
	// What names no repository is not asked.
	before := asked
	if _, err = client.ReadGitHubConnection(ctx, "not-a-repository"); err == nil || asked != before {
		t.Fatalf("an identifier that names no repository: %v, %d requests", err, asked-before)
	}
}
