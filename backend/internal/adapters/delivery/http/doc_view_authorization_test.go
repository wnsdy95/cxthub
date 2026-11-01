package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type docViewAuthorizationBackend struct {
	Backend
	read func(context.Context) error
}

func (backend *docViewAuthorizationBackend) ReadDocViewEvents(ctx context.Context, _, _, _ domain.ContentHash, _, _ int, _ string) (domain.DocViewPage, error) {
	return domain.DocViewPage{Next: -1}, backend.read(ctx)
}

func (backend *docViewAuthorizationBackend) ReadDocViewFragment(ctx context.Context, _, _ domain.ContentHash, _, _, _ int) (domain.DocViewFragment, error) {
	return domain.DocViewFragment{Next: -1}, backend.read(ctx)
}

func TestDocViewHTTPDeferredAuthorization(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	for _, endpoint := range []struct {
		name string
		path string
	}{
		{name: "chat", path: "/events?view=chat"},
		{name: "fragment", path: "/event-fragments?index=0"},
	} {
		for _, change := range []struct {
			name    string
			status  int
			code    string
			actor   string
			lookups int
		}{
			{name: "current identity", status: http.StatusOK, actor: "current", lookups: 2},
			{name: "revoked token", status: http.StatusUnauthorized, code: "unauthenticated", lookups: 2},
			{name: "revoked role", status: http.StatusForbidden, code: "forbidden", actor: "current", lookups: 2},
			{name: "foreign repo", status: http.StatusForbidden, code: "forbidden", lookups: 1},
		} {
			test.Run(endpoint.name+"/"+change.name, func(test *testing.T) {
				policy := &docVisiblePolicyBackend{repo: domain.Repo{ID: repo, RepositoryID: "repository"}}
				identity := &docVisiblePolicyIdentity{
					repository: domain.Repository{ID: "repository", Visibility: domain.VisibilityPrivate},
					user:       domain.User{ID: "before-admission"},
					role:       domain.RoleViewer,
				}
				backend := &docViewAuthorizationBackend{Backend: policy}
				backendCalls, bodyReads := 0, 0
				backend.read = func(ctx context.Context) error {
					backendCalls++
					if policy.calls != 1 || identity.resolveCalls != 1 {
						test.Fatal("initial guard missing or deferred authorization ran before backend admission", policy.calls, identity.resolveCalls)
					}
					authorize := inbound.DocumentReadAuthorizer(ctx)
					if authorize == nil {
						test.Fatal("backend context has no document read authorizer")
					}
					user, authed := userFrom(ctx)
					if !authed || user.ID != "before-admission" {
						test.Fatal("initial guard did not populate the stale request identity", user, authed)
					}
					identity.user = domain.User{ID: "current"}
					requestedRepo := repo
					switch change.name {
					case "revoked token":
						identity.credentialErr = domain.ErrUnauthorized
					case "revoked role":
						identity.role = ""
					case "foreign repo":
						requestedRepo = domain.HashContent([]byte("foreign repo"))
					}
					current, err := authorize(inbound.WithSystemActor(ctx), requestedRepo)
					if current == nil {
						test.Fatal("document read authorizer returned no context", err)
					}
					actor, system := inbound.RepositoryActor(current)
					user, authed = userFrom(current)
					if system || actor != change.actor || authed != (change.actor != "") || user.ID != change.actor {
						test.Fatalf("stale authority retained: actor=%q want=%q system=%v user=%+v authed=%v err=%v", actor, change.actor, system, user, authed, err)
					}
					if err == nil {
						bodyReads++
					}
					return err
				}
				request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+string(repo)+"/docs/"+string(hash)+endpoint.path, nil)
				request.Header.Set("Authorization", "Bearer session")
				writer := httptest.NewRecorder()
				newDocumentViewTestServer(backend, identity).Handler().ServeHTTP(writer, request)
				wantReads := 0
				if change.status == http.StatusOK {
					wantReads = 1
				}
				if writer.Code != change.status || backendCalls != 1 || bodyReads != wantReads {
					test.Fatalf("deferred access changed: status=%d want=%d backend=%d reads=%d want=%d body=%s", writer.Code, change.status, backendCalls, bodyReads, wantReads, writer.Body)
				}
				if change.code != "" && !strings.Contains(writer.Body.String(), `"code":"`+change.code+`"`) {
					test.Fatalf("authorization error code changed: want=%q body=%s", change.code, writer.Body)
				}
				if policy.calls != change.lookups || identity.resolveCalls != change.lookups {
					test.Fatalf("repository or credential policy was not rechecked within request scope: repos=%d credentials=%d want=%d", policy.calls, identity.resolveCalls, change.lookups)
				}
			})
		}
	}
}
