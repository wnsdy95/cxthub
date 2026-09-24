package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{AppID: "42", ClientID: "client", ClientSecret: "secret", Slug: "cxthub", PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), CallbackURL: "https://example.test/api/v1/github/callback"})
	if err != nil {
		t.Fatal(err)
	}
	c.api = srv.URL
	c.web = srv.URL
	return c
}
func TestAuthorizationRequiresInstallationAndOrganizationAdmin(t *testing.T) {
	var admin atomic.Bool
	var visible atomic.Bool
	visible.Store(true)
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			var v map[string]string
			if json.NewDecoder(r.Body).Decode(&v) != nil || v["code_verifier"] != "proof" || v["client_secret"] != "secret" {
				t.Error("invalid exchange")
			}
			fmt.Fprint(w, `{"access_token":"user-token"}`)
		case "/user":
			if r.Header.Get("Authorization") != "Bearer user-token" {
				t.Error("wrong user token")
			}
			fmt.Fprint(w, `{"id":1,"login":"operator"}`)
		case "/user/installations":
			if visible.Load() {
				fmt.Fprint(w, `{"installations":[{"id":9}]}`)
			} else {
				fmt.Fprint(w, `{"installations":[]}`)
			}
		case "/app/installations/9":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
				t.Error("not app JWT")
			}
			fmt.Fprint(w, `{"id":9,"app_id":42,"account":{"id":20,"login":"org","type":"Organization"}}`)
		case "/user/memberships/orgs/org":
			role := "member"
			if admin.Load() {
				role = "admin"
			}
			fmt.Fprintf(w, `{"role":%q,"state":"active","organization":{"id":20}}`, role)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	if _, err := c.Authorize(context.Background(), "code", "proof", 9); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("ordinary member authorized: %v", err)
	}
	admin.Store(true)
	p, err := c.Authorize(context.Background(), "code", "proof", 9)
	if err != nil || p.Identity.ExternalID != 1 || p.Installation.AccountID != 20 {
		t.Fatalf("admin proof %+v %v", p, err)
	}
	visible.Store(false)
	if _, err = c.Authorize(context.Background(), "code", "proof", 9); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("spoofed install: %v", err)
	}
	u, _ := url.Parse(c.AuthorizeURL("state", "proof"))
	sum := sha256.Sum256([]byte("proof"))
	if u.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("PKCE missing")
	}
}
func TestInstallationTokenRefreshIsScopedAndCoalesced(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" {
			t.Error("unexpected request")
		}
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, r.URL.Path, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			if _, err := c.InstallationToken(context.Background(), 9); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh stampede %d", calls.Load())
	}
	a, _ := c.InstallationToken(context.Background(), 9)
	b, _ := c.InstallationToken(context.Background(), 10)
	if a == b || calls.Load() != 2 {
		t.Fatal("installation credentials crossed")
	}
	c.Invalidate(9)
	if _, err := c.InstallationToken(context.Background(), 9); err != nil || calls.Load() != 3 {
		t.Fatal("invalidation did not refresh")
	}
}
func TestRateLimitDoesNotBlockOtherInstallation(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/limited" {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	if err := c.request(context.Background(), "GET", c.api+"/limited", "", "installation:1", nil, &struct{}{}); err == nil {
		t.Fatal("rate limit ignored")
	}
	if err := c.request(context.Background(), "GET", c.api+"/ok", "", "installation:2", nil, &struct{}{}); err != nil {
		t.Fatalf("other installation blocked: %v", err)
	}
}
func TestGitHubDoesNotFollowCredentialRedirects(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) })
	if err := c.request(context.Background(), "GET", c.api+"/user", "sensitive", "oauth", nil, &struct{}{}); err == nil {
		t.Fatal("redirect accepted")
	}
	if leaked.Load() {
		t.Fatal("credentials followed redirect")
	}
}
