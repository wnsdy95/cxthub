//go:build postgres

package federation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	delivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/http"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type flowDNS struct{ value string }

func (d *flowDNS) LookupTXT(context.Context, string) ([]string, error) { return []string{d.value}, nil }

func TestPGSignedOIDCBrowserCallbackAndReplay(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	ctx := context.Background()
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	f := newOIDCFixture(t)
	dns := &flowDNS{}
	vault, _ := NewVault(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("y", 32))))
	ids := app.NewIdentityService(auth.NewDevVerifier(), st).WithDomainResolver(dns).WithOIDC(f.client, vault, "https://cxthub.example.test")
	u, sess, err := ids.Login(ctx, "dev:"+domain.NewID("flow")+"@example.test", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	e, err := ids.CreateEnterprise(ctx, u, "OIDC flow", "flow-"+domain.NewID("")[:12])
	if err != nil {
		t.Fatal(err)
	}
	d, err := ids.RequestEnterpriseDomain(ctx, u.ID, e.ID, domain.NewID("")+".example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	dns.value = d.Challenge
	if _, err = ids.VerifyEnterpriseDomain(ctx, u.ID, e.ID, d.Domain, d.Revision); err != nil {
		t.Fatal(err)
	}
	server := delivery.NewServer(app.NewService(st, st, nil, gitengine.NewEngine(st), st), ids).Handler()
	request := func(method, path string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.AddCookie(&http.Cookie{Name: "cxt_session", Value: sess.Token})
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Cxt-CSRF", "1")
		r.Header.Set("Origin", "http://example.com")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		return w
	}
	endpoint := "/api/v1/enterprises/" + e.ID + "/oidc"
	w := request("POST", endpoint, app.OIDCConnectionInput{Domain: d.Domain, Issuer: f.settings.Issuer, ClientID: f.settings.ClientID, ClientSecret: f.settings.ClientSecret, AuthMethod: f.settings.AuthMethod})
	if w.Code != 200 {
		t.Fatal("configure", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), f.settings.ClientSecret) {
		t.Fatal("secret returned")
	}
	w = request("POST", endpoint+"/authorize", map[string]any{})
	if w.Code != 200 {
		t.Fatal("authorize", w.Code, w.Body.String())
	}
	var start app.OIDCAuthorization
	if json.Unmarshal(w.Body.Bytes(), &start) != nil {
		t.Fatal("bad start")
	}
	uURL, _ := url.Parse(start.URL)
	state := uURL.Query().Get("state")
	nonce := uURL.Query().Get("nonce")
	f.claims["nonce"] = nonce
	a, err := st.GetOIDCAttempt(ctx, domain.HashToken(state))
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := vault.Open(a.Hash, a.Verifier)
	if err != nil {
		t.Fatal(err)
	}
	f.expectedVerifier = verifier
	callback := "/api/v1/auth/enterprise/oidc/callback?state=" + url.QueryEscape(state) + "&code=synthetic-code"
	w = request("GET", callback, nil)
	if w.Code != 303 || w.Header().Get("Location") != "/enterprises/"+e.Slug+"?tab=identity" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("callback", w.Code, w.Body.String())
	}
	w = request("GET", callback, nil)
	if w.Code != 401 {
		t.Fatal("replay", w.Code)
	}
	w = request("GET", endpoint, nil)
	var view app.OIDCView
	if json.Unmarshal(w.Body.Bytes(), &view) != nil || !view.Linked || view.VerifiedUntil == nil {
		t.Fatal("session proof", w.Code, w.Body.String())
	}
	// Recovery uses the real signed callback above. It must not accept only a
	// claimed browser identity or turn recovery evidence into SSO verification.
	root := "/api/v1/enterprises/" + e.ID
	recovery := root + "/owner-recovery"
	var current app.OwnerRecoveryView
	w = request("GET", recovery, nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &current) != nil || current.Ready {
		t.Fatal("initial recovery", w.Code, w.Body.String())
	}
	w = request("POST", recovery+"/prepare", map[string]string{"revision": current.Revision})
	var prepared app.PreparedOwnerRecovery
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &prepared) != nil || len(prepared.Code) != 56 {
		t.Fatal("prepare recovery", w.Code)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("recovery secret response can be retained")
	}
	w = request("POST", recovery+"/confirm", map[string]string{"revision": prepared.Revision, "code": prepared.Code})
	if w.Code != 200 {
		t.Fatal("confirm recovery", w.Code, w.Body.String())
	}
	w = request("GET", recovery, nil)
	if json.Unmarshal(w.Body.Bytes(), &current) != nil || !current.Ready || strings.Contains(w.Body.String(), prepared.Code) || strings.Contains(w.Body.String(), "orh_") {
		t.Fatal("confirmed recovery view leaks or loses state")
	}
	// A saved code remains usable during provider unavailability, but does not
	// manufacture federation approval. Disabling the connection models this.
	if err = ids.DisableOIDC(ctx, u.ID, e.ID, view.Connection.Revision); err != nil {
		t.Fatal(err)
	}
	w = request("POST", recovery+"/redeem", map[string]string{"code": prepared.Code})
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &current) != nil || current.RepairUntil == nil || current.Ready {
		t.Fatal("redeem recovery", w.Code, w.Body.String())
	}
	w = request("GET", root+"/credential-assessment", nil)
	var proof app.CredentialAssessment
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &proof) != nil || proof.State == "verified" {
		t.Fatal("recovery must not restore SSO approval", w.Code, w.Body.String())
	}
	w = request("POST", recovery+"/redeem", map[string]string{"code": prepared.Code})
	if w.Code != 401 {
		t.Fatal("recovery replay", w.Code)
	}
}
