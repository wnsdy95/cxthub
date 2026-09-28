//go:build postgres

package federation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	dsig "github.com/russellhaering/goxmldsig"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	delivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/http"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type samlGate struct {
	*SAML
	afterVerify func()
	afterKeys   func()
}

func (p *samlGate) Keys(ctx context.Context) (string, string, error) {
	c, k, err := p.SAML.Keys(ctx)
	if p.afterKeys != nil {
		p.afterKeys()
	}
	return c, k, err
}

func (p *samlGate) Verify(ctx context.Context, s outbound.SAMLSettings, id string, raw []byte, at time.Time) (outbound.SAMLProof, error) {
	proof, err := p.SAML.Verify(ctx, s, id, raw, at)
	if p.afterVerify != nil {
		p.afterVerify()
	}
	return proof, err
}

type samlFlow struct {
	st       *store.PostgresStore
	ids      *app.IdentityService
	provider *samlGate
	f        samlFixture
	user     domain.User
	session  domain.Session
	ep       domain.Enterprise
	view     app.SAMLView
	handler  http.Handler
	domain   app.EnterpriseDomainView
}

func newSAMLFlow(t *testing.T) *samlFlow {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	ctx := context.Background()
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err = st.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	f := newSAMLFixture(t)
	p := &samlGate{SAML: NewSAML()}
	dns := &flowDNS{}
	vault, _ := NewVault(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("z", 32))))
	ids := app.NewIdentityService(auth.NewDevVerifier(), st).WithDomainResolver(dns).WithSAML(p, vault, "https://cxthub.example.test")
	user, sess, err := ids.Login(ctx, "dev:"+domain.NewID("saml")+"@example.test", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	ep, err := ids.CreateEnterprise(ctx, user, "SAML flow", "saml-"+domain.NewID("")[:12])
	if err != nil {
		t.Fatal(err)
	}
	d, err := ids.RequestEnterpriseDomain(ctx, user.ID, ep.ID, domain.NewID("")+".example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	dns.value = d.Challenge
	d, err = ids.VerifyEnterpriseDomain(ctx, user.ID, ep.ID, d.Domain, d.Revision)
	if err != nil {
		t.Fatal(err)
	}
	view, err := ids.ConfigureSAML(ctx, user.ID, ep.ID, app.SAMLConnectionInput{Domain: d.Domain, Metadata: f.settings.Metadata})
	if err != nil {
		t.Fatal(err)
	}
	f.settings.EntityID, f.settings.ACS = view.EntityID, view.ACS
	handler := delivery.NewServer(app.NewService(st, st, nil, gitengine.NewEngine(st), st), ids).Handler()
	return &samlFlow{st, ids, p, f, user, sess, ep, view, handler, d}
}
func (f *samlFlow) start(t *testing.T, assertionID string) (string, []byte) {
	t.Helper()
	ctx := context.Background()
	auth, err := f.ids.BeginSAML(ctx, f.user.ID, f.ep.ID, f.session.Token)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(auth.URL)
	state := u.Query().Get("RelayState")
	a, err := f.st.GetSAMLAttempt(ctx, domain.HashToken(state))
	if err != nil {
		t.Fatal(err)
	}
	assertion := f.f.assertion()
	assertion.ID = assertionID
	assertion.Subject.SubjectConfirmations[0].SubjectConfirmationData.InResponseTo = a.RequestID
	return state, f.f.responseID(t, assertion, dsig.RSASHA256SignatureMethod, a.RequestID)
}
func (f *samlFlow) request(method, path, token, body, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.AddCookie(&http.Cookie{Name: "cxt_session", Value: token})
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func TestPGSAMLBrowserReceiptAndCompletion(t *testing.T) {
	f := newSAMLFlow(t)
	ctx := context.Background()
	state, raw := f.start(t, domain.NewID("assert_"))
	acs := "/api/v1/auth/enterprise/saml/" + f.ep.ID + "/acs"
	body := url.Values{"RelayState": {state}, "SAMLResponse": {base64.StdEncoding.EncodeToString(raw)}}.Encode()
	// Even an ambient cookie has no authority at the cross-site POST endpoint.
	w := f.request("POST", acs, "unrelated-cookie", body, "application/x-www-form-urlencoded")
	if w.Code != 303 {
		t.Fatal("receipt", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("callback headers")
	}
	finish := w.Header().Get("Location")
	if strings.Contains(finish, "SAMLResponse") || strings.Contains(finish, "immutable-subject") {
		t.Fatal("response leaked")
	}
	if _, err := f.st.GetFederationIdentity(ctx, f.ep.ID, "saml", f.user.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("cross-site receipt linked an account", err)
	}
	if f.request("GET", finish, "", "", "").Code != 401 || f.request("GET", finish, "sess_wrong", "", "").Code != 401 {
		t.Fatal("wrong browser accepted")
	}
	w = f.request("GET", finish, f.session.Token, "", "")
	if w.Code != 303 || w.Header().Get("Location") != "/enterprises/"+f.ep.Slug+"?tab=identity" {
		t.Fatal("finish", w.Code, w.Body.String())
	}
	if f.request("GET", finish, f.session.Token, "", "").Code != 401 || f.request("POST", acs, "", body, "application/x-www-form-urlencoded").Code != 401 {
		t.Fatal("replay accepted")
	}
	view, err := f.ids.GetSAMLView(ctx, f.user.ID, f.ep.ID, f.session.Token)
	if err != nil || !view.Linked || view.VerifiedUntil == nil {
		t.Fatal("missing proof", err)
	}
	// A fresh credential of the same user is not verified by another browser.
	_, other, err := f.ids.Login(ctx, "dev:"+f.user.Email, "second browser")
	if err != nil {
		t.Fatal(err)
	}
	view, err = f.ids.GetSAMLView(ctx, f.user.ID, f.ep.ID, other.Token)
	if err != nil || view.VerifiedUntil != nil {
		t.Fatal("proof widened to another browser", err)
	}
	b, _ := json.Marshal(view)
	if bytes.Contains(b, []byte("PRIVATE KEY")) || bytes.Contains(b, []byte("immutable-subject")) {
		t.Fatal("private material in view")
	}
	// Ordinary management mutations still require same-origin CSRF headers.
	w = f.request("POST", "/api/v1/enterprises/"+f.ep.ID+"/saml/disable", f.session.Token, `{"revision":"`+f.view.Connection.Revision+`"}`, "application/json")
	if w.Code != 403 {
		t.Fatal("CSRF exemption widened", w.Code)
	}
	md := f.request("GET", f.view.EntityID, "", "", "")
	if md.Code != 200 || strings.Contains(md.Body.String(), "PRIVATE KEY") {
		t.Fatal("public metadata", md.Code)
	}
	c, err := f.st.GetSAMLConnection(ctx, f.ep.ID)
	if err != nil || strings.Contains(c.PrivateKey, "PRIVATE KEY") || !strings.HasPrefix(c.PrivateKey, "v1.") {
		t.Fatal("key not encrypted", err)
	}
}
func TestPGSAMLConcurrentCallbacksAndAssertionReplay(t *testing.T) {
	f := newSAMLFlow(t)
	ctx := context.Background()
	assertionID := domain.NewID("assert_")
	state, raw := f.start(t, assertionID)
	finishes := make(chan string, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			finish, err := f.ids.ReceiveSAML(ctx, f.ep.ID, state, raw)
			if err == nil {
				finishes <- finish
			}
		}()
	}
	wg.Wait()
	close(finishes)
	var finish string
	count := 0
	for v := range finishes {
		count++
		finish = v
	}
	if count != 1 {
		t.Fatal("receipt count", count)
	}
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := f.ids.CompleteSAML(ctx, f.session.Token, finish); results <- err }()
	}
	wg.Wait()
	close(results)
	count = 0
	for err := range results {
		if err == nil {
			count++
		}
	}
	if count != 1 {
		t.Fatal("completion count", count)
	}
	state, raw = f.start(t, assertionID)
	if _, err := f.ids.ReceiveSAML(ctx, f.ep.ID, state, raw); err == nil {
		t.Fatal("assertion reused across request IDs")
	}
}
func TestPGSAMLRechecksRevocationAtReceiptAndFinish(t *testing.T) {
	for _, phase := range []string{"during verification", "before completion"} {
		t.Run(phase, func(t *testing.T) {
			f := newSAMLFlow(t)
			ctx := context.Background()
			state, raw := f.start(t, domain.NewID("assert_"))
			disable := func() {
				if err := f.ids.DisableSAML(ctx, f.user.ID, f.ep.ID, f.view.Connection.Revision); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "during verification" {
				f.provider.afterVerify = disable
			}
			finish, err := f.ids.ReceiveSAML(ctx, f.ep.ID, state, raw)
			if phase == "during verification" {
				if err == nil {
					t.Fatal("disabled connection published proof")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				disable()
				if _, err = f.ids.CompleteSAML(ctx, f.session.Token, finish); err == nil {
					t.Fatal("disabled connection linked account")
				}
			}
			if _, err = f.st.GetFederationIdentity(ctx, f.ep.ID, "saml", f.user.ID); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("binding survived revoked attempt", err)
			}
		})
	}
}

func TestPGSAMLDomainAndSessionRevocationBeforeCompletion(t *testing.T) {
	for _, cause := range []string{"domain", "session", "revision"} {
		t.Run(cause, func(t *testing.T) {
			f := newSAMLFlow(t)
			ctx := context.Background()
			state, raw := f.start(t, domain.NewID("assert_"))
			finish, err := f.ids.ReceiveSAML(ctx, f.ep.ID, state, raw)
			if err != nil {
				t.Fatal(err)
			}
			switch cause {
			case "domain":
				err = f.ids.ReleaseEnterpriseDomain(ctx, f.user.ID, f.ep.ID, f.domain.Domain, f.domain.Revision)
			case "session":
				err = f.ids.Logout(ctx, f.session.Token)
			case "revision":
				var view app.SAMLView
				view, err = f.ids.ConfigureSAML(ctx, f.user.ID, f.ep.ID, app.SAMLConnectionInput{Domain: f.domain.Domain, Metadata: f.f.settings.Metadata, Revision: f.view.Connection.Revision})
				if err == nil && view.Connection.Certificate != f.view.Connection.Certificate {
					t.Fatal("metadata update rotated SP key unexpectedly")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.ids.CompleteSAML(ctx, f.session.Token, finish); err == nil {
				t.Fatal("revoked proof completed")
			}
			if _, err = f.st.GetFederationIdentity(ctx, f.ep.ID, "saml", f.user.ID); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("revoked proof linked account", err)
			}
		})
	}
}

func TestPGSAMLCompletionAfterServiceRestart(t *testing.T) {
	f := newSAMLFlow(t)
	ctx := context.Background()
	state, raw := f.start(t, domain.NewID("assert_"))
	finish, err := f.ids.ReceiveSAML(ctx, f.ep.ID, state, raw)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.NewPostgresStore(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	vault, _ := NewVault(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("z", 32))))
	ids := app.NewIdentityService(auth.NewDevVerifier(), other).WithSAML(NewSAML(), vault, "https://cxthub.example.test")
	if _, err = ids.CompleteSAML(ctx, f.session.Token, finish); err != nil {
		t.Fatal("durable completion lost", err)
	}
	if _, err = f.ids.CompleteSAML(ctx, f.session.Token, finish); err == nil {
		t.Fatal("old process accepted replay")
	}
}

func TestPGSAMLMembershipRevokedDuringValidation(t *testing.T) {
	f := newSAMLFlow(t)
	ctx := context.Background()
	owner := f.user
	u, sess, err := f.ids.Login(ctx, "dev:"+domain.NewID("member")+"@example.test", "member")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.ids.UpdateEnterpriseMember(ctx, owner.ID, f.ep.ID, u.ID, domain.EnterpriseMember); err != nil {
		t.Fatal(err)
	}
	f.user, f.session = u, sess
	if _, err = f.ids.ConfigureSAML(ctx, u.ID, f.ep.ID, app.SAMLConnectionInput{Domain: f.domain.Domain, Metadata: f.f.settings.Metadata, Revision: f.view.Connection.Revision}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("member configured connection", err)
	}
	state, raw := f.start(t, domain.NewID("assert_"))
	f.provider.afterVerify = func() {
		if err := f.ids.RemoveEnterpriseMember(ctx, owner.ID, f.ep.ID, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = f.ids.ReceiveSAML(ctx, f.ep.ID, state, raw); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("revoked member published proof", err)
	}
	if _, err = f.st.GetFederationIdentity(ctx, f.ep.ID, "saml", u.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("removed member linked", err)
	}
}
