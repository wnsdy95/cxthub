package http

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type repositoryGitHubConnections struct {
	GitHubConnections
	asked     []domain.ContentHash
	connected bool
	err       error
}

func (g *repositoryGitHubConnections) RepositoryConnection(_ context.Context, repo domain.ContentHash) (bool, error) {
	g.asked = append(g.asked, repo)
	return g.connected, g.err
}

// Whoever may view a repository is told whether the server reads it on GitHub
// through the GitHub App, and where a maintainer connects it.
func TestGitHubRepositoryConnectionHTTP(t *testing.T) {
	const page = "https://example.test/connect/github"
	for _, test := range []struct {
		name      string
		method    string
		role      domain.MemberRole
		authed    bool
		app       bool
		connected bool
		err       error
		status    int
		answer    map[string]any
	}{
		{name: "anonymous", method: http.MethodGet, role: domain.RoleOwner, app: true, status: 401},
		{name: "no_role", method: http.MethodGet, authed: true, app: true, status: 403},
		{name: "writing_it", method: http.MethodPost, role: domain.RoleOwner, authed: true, app: true, status: 405},
		{name: "a_server_without_the_app", method: http.MethodGet, role: domain.RoleViewer, authed: true, status: 200,
			answer: map[string]any{"state": "not_configured"}},
		{name: "connected", method: http.MethodGet, role: domain.RoleViewer, authed: true, app: true, connected: true, status: 200,
			answer: map[string]any{"state": "connected", "connect_url": page}},
		{name: "not_connected", method: http.MethodGet, role: domain.RoleMember, authed: true, app: true, status: 200,
			answer: map[string]any{"state": "not_connected", "connect_url": page}},
		{name: "the_service_failed", method: http.MethodGet, role: domain.RoleViewer, authed: true, app: true, connected: true, err: domain.ErrConflict, status: 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &collectorHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte(t.Name())), RepositoryID: "ws_" + strings.Repeat("e", 32)}}
			server := NewServer(backend, branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: test.role})
			connections := &repositoryGitHubConnections{connected: test.connected, err: test.err}
			if test.app {
				server.SetGitHubConnections(connections, "fixture-secret", page)
			}
			body := ""
			if test.method == http.MethodPost {
				body = "{}"
			}
			response := bpHTTP(t, server.Handler(), test.method, "/api/v1/repos/"+string(backend.repo.ID)+"/github-connection", body, test.authed)
			if response.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
			}
			reached := test.app && test.status != 401 && test.status != 403 && test.status != 405
			if reached != reflect.DeepEqual(connections.asked, []domain.ContentHash{backend.repo.ID}) || (!reached && len(connections.asked) != 0) {
				t.Fatalf("the service was asked for %v", connections.asked)
			}
			if test.answer == nil {
				if strings.Contains(response.Body.String(), "connect") {
					t.Fatalf("a refusal carried the answer: %s", response.Body.String())
				}
				return
			}
			var answer map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil || !reflect.DeepEqual(answer, test.answer) {
				t.Fatalf("the answer: %s %v", response.Body.String(), err)
			}
		})
	}
}
