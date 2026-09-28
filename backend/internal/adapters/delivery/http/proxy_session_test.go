package http

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
)

// The browser's public origin differs from the upstream Host after a proxy
// rewrite. These are the API values required by check-render-blueprint.py.
// Only the IDP is replaced by a synthetic verifier; sessions and CSRF use the
// real handler and store. No external authentication service is contacted.
func TestProductionProxySessionBoundary(t *testing.T) {
	const origin = "https://cxthub.example"
	const upstream = "https://api.example.onrender.com"
	t.Setenv("CXT_PUBLIC_URL", origin)
	t.Setenv("CXT_CORS_ORIGINS", origin)
	t.Setenv("CXT_COOKIE_SECURE", "1")
	t.Setenv("CXT_COOKIE_SAMESITE", "")
	t.Setenv("CXT_COOKIE_DOMAIN", "")
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	identity := app.NewIdentityService(auth.NewDevVerifier(), st)
	handler := NewServer(svc, identity).Handler()

	login := httptest.NewRequest(http.MethodPost, upstream+"/api/v1/auth/session", nil)
	login.Header.Set("Authorization", "Bearer dev:proxy@example.test:Proxy")
	login.Header.Set("Origin", origin)
	login.Header.Set("X-Forwarded-Proto", "https")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, login)
	if response.Code != http.StatusOK {
		t.Fatalf("login status=%d", response.Code)
	}
	var session *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookie {
			session = cookie
		}
	}
	assertCookie := func(c *http.Cookie) {
		t.Helper()
		if c == nil || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Domain != "" || c.Path != "/" {
			t.Fatal("session cookie must be Secure, HttpOnly, SameSite=Lax and host-only")
		}
	}
	assertCookie(session)
	if strings.Contains(response.Body.String(), session.Value) {
		t.Fatal("session token exposed to JavaScript")
	}

	for _, tc := range []struct {
		name, origin, csrf string
		want               int
	}{
		{"public origin", origin, "1", http.StatusOK},
		{"foreign origin", "https://other.example", "1", http.StatusForbidden},
		{"lookalike origin", "https://cxthub.example.attacker.test", "1", http.StatusForbidden},
		{"HTTP downgrade", "http://cxthub.example", "1", http.StatusForbidden},
		{"different port", "https://cxthub.example:444", "1", http.StatusForbidden},
		{"loopback", "http://localhost:5173", "1", http.StatusForbidden},
		{"missing origin", "", "1", http.StatusForbidden},
		{"missing CSRF header", origin, "", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, upstream+"/api/v1/repositories", bytes.NewBufferString(`{"name":"ProxyRepository"}`))
			req.AddCookie(session)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("X-Cxt-CSRF", tc.csrf)
			req.Header.Set("X-Forwarded-Proto", "https")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("status=%d, want %d: %s", res.Code, tc.want, res.Body.String())
			}
			if tc.origin != origin && res.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("untrusted origin received credentialed CORS access")
			}
		})
	}

	logout := httptest.NewRequest(http.MethodDelete, upstream+"/api/v1/auth/session", nil)
	logout.AddCookie(session)
	logout.Header.Set("Origin", origin)
	logout.Header.Set("X-Cxt-CSRF", "1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, logout)
	if response.Code != http.StatusOK {
		t.Fatalf("logout status=%d", response.Code)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie || cookies[0].MaxAge != -1 {
		t.Fatal("logout did not expire the session cookie")
	}
	assertCookie(cookies[0])
	me := httptest.NewRequest(http.MethodGet, upstream+"/api/v1/me", nil)
	me.AddCookie(session)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, me)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out session status=%d", response.Code)
	}
}
