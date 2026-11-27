package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type branchPullHTTPIdentity struct {
	IdentityBackend
	repo       domain.Repository
	role       domain.MemberRole
	breakGlass bool
}

func (i branchPullHTTPIdentity) GetRepository(context.Context, string) (domain.Repository, error) {
	return i.repo, nil
}
func (i branchPullHTTPIdentity) ResolveUser(context.Context, string) (domain.User, error) {
	return domain.User{ID: "actor"}, nil
}
func (i branchPullHTTPIdentity) RoleOf(context.Context, string, string) (domain.MemberRole, bool) {
	return i.role, i.role != ""
}
func (i branchPullHTTPIdentity) HasBreakGlassAccess(context.Context, string, string) (bool, error) {
	return i.breakGlass, nil
}
func bpHTTP(t *testing.T, h http.Handler, method, path, body string, authed bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if authed {
		r.Header.Set("Authorization", "Bearer fixture-token")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
