//go:build postgres

package federation

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func signingChange(t *testing.T, f *samlFlow, action string) app.SAMLView {
	t.Helper()
	view, err := f.ids.ChangeSAMLSigning(context.Background(), f.user.ID, f.ep.ID, app.SAMLSigningInput{Revision: f.view.Connection.Revision, Action: action, TrustConfirmed: true})
	if err != nil {
		t.Fatal(action, err)
	}
	f.view = view
	return view
}
func TestPGSAMLSigningHTTPRevisionAndOwnerContract(t *testing.T) {
	f := newSAMLFlow(t)
	ctx := context.Background()
	path := "/api/v1/enterprises/" + f.ep.ID + "/saml/signing"
	post := func(token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://example.com")
		r.Header.Set("X-Cxt-CSRF", "1")
		if token != "" {
			r.AddCookie(&http.Cookie{Name: "cxt_session", Value: token})
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}

	input := app.SAMLSigningInput{Revision: f.view.Connection.Revision, Action: "prepare"}
	raw, _ := json.Marshal(input)
	if w := f.request("POST", path, f.session.Token, string(raw), "application/json"); w.Code != 403 {
		t.Fatal("missing CSRF accepted", w.Code)
	}
	if w := post("", string(raw)); w.Code != 401 {
		t.Fatal("anonymous", w.Code)
	}
	_, other, err := f.ids.Login(ctx, "dev:"+domain.NewID("")+"@example.test", "other")
	if err != nil {
		t.Fatal(err)
	}
	if w := post(other.Token, string(raw)); w.Code != 403 {
		t.Fatal("non-owner", w.Code)
	}
	w := post(f.session.Token, string(raw))
	if w.Code != 200 {
		t.Fatal("prepare", w.Code, w.Body.String())
	}
	var prepared app.SAMLView
	if err = json.Unmarshal(w.Body.Bytes(), &prepared); err != nil || prepared.Connection == nil || prepared.Connection.Rotation == nil {
		t.Fatal("missing rotation", err)
	}
	if w = post(f.session.Token, string(raw)); w.Code != 409 {
		t.Fatal("stale edit", w.Code)
	}
	input.Revision = prepared.Connection.Revision
	input.Action = "activate"
	raw, _ = json.Marshal(input)
	if w = post(f.session.Token, string(raw)); w.Code != 422 {
		t.Fatal("unconfirmed activation", w.Code)
	}
	current, err := f.st.GetSAMLConnection(ctx, f.ep.ID)
	if err != nil || current.Rotation.State != "prepared" {
		t.Fatal("failed activation changed state", err)
	}
}
func verifyRequestCertificate(t *testing.T, f *samlFlow, certPEM string) {
	t.Helper()
	a, err := f.ids.BeginSAML(context.Background(), f.user.ID, f.ep.ID, f.session.Token)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(a.URL)
	q := u.Query()
	input := "SAMLRequest=" + url.QueryEscape(q.Get("SAMLRequest")) + "&RelayState=" + url.QueryEscape(q.Get("RelayState")) + "&SigAlg=" + url.QueryEscape(q.Get("SigAlg"))
	sum := sha256.Sum256([]byte(input))
	sig, err := base64.StdEncoding.DecodeString(q.Get("Signature"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode([]byte(certPEM))
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err = rsa.VerifyPKCS1v15(c.PublicKey.(*rsa.PublicKey), crypto.SHA256, sum[:], sig); err != nil {
		t.Fatal("request signed with wrong key", err)
	}
}
func signingRoundtrip(t *testing.T, f *samlFlow) {
	t.Helper()
	ctx := context.Background()
	state, raw := f.start(t, domain.NewID("assert_"))
	finish, err := f.ids.ReceiveSAML(ctx, f.ep.ID, state, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.ids.CompleteSAML(ctx, f.session.Token, finish); err != nil {
		t.Fatal(err)
	}
	f.view, err = f.ids.GetSAMLView(ctx, f.user.ID, f.ep.ID, f.session.Token)
	if err != nil {
		t.Fatal(err)
	}
}
func signingVault(t *testing.T, legacy bool) *Vault {
	t.Helper()
	old := ""
	if legacy {
		old = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("z", 32)))
	}
	cfg, _ := json.Marshal(map[string]any{"active": "next", "keys": map[string]string{"next": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("n", 32)))}})
	v, err := ConfiguredVault(old, string(cfg))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestPGSAMLSigningRolloverAndEncryptionRewrap(t *testing.T) {
	f := newSAMLFlow(t)
	ctx := context.Background()
	original := f.view.Connection.Certificate
	// The old revision is a real outstanding browser request, not a fixture flag.
	state, raw := f.start(t, domain.NewID("old_"))
	prepared := signingChange(t, f, "prepare")
	if prepared.Connection.Certificate != original || prepared.Connection.Rotation.State != "prepared" {
		t.Fatal("preparation switched active key")
	}
	verifyRequestCertificate(t, f, original)
	if _, err := f.ids.ReceiveSAML(ctx, f.ep.ID, state, raw); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("old revision accepted", err)
	}
	md, err := f.ids.SAMLMetadata(ctx, f.ep.ID)
	if err != nil || strings.Count(md, `use="signing"`) != 2 || strings.Contains(md, "PRIVATE KEY") {
		t.Fatal("rollover metadata", err)
	}
	updated, err := f.ids.ConfigureSAML(ctx, f.user.ID, f.ep.ID, app.SAMLConnectionInput{Domain: f.domain.Domain, Metadata: f.f.settings.Metadata, Revision: prepared.Connection.Revision})
	if err != nil || updated.Connection.Rotation == nil || updated.Connection.Rotation.ID != prepared.Connection.Rotation.ID {
		t.Fatal("metadata erased rotation", err)
	}
	prepared = updated
	f.view = updated
	if _, err = f.ids.ChangeSAMLSigning(ctx, f.user.ID, f.ep.ID, app.SAMLSigningInput{Revision: prepared.Connection.Revision, Action: "activate"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("unconfirmed activation", err)
	}
	// Both active and staged private keys participate in encryption lifecycle.
	batch, err := app.RewrapIdentityKeys(ctx, f.st, signingVault(t, true), app.IdentityKeyRequest{EnterpriseID: f.ep.ID, Operation: domain.NewID("rewrap_"), Actor: "operator", Reason: "fixture", Limit: 100, Apply: true})
	if err != nil || batch.Changed != 2 {
		t.Fatal("alternate omitted from rewrap", batch, err)
	}
	f.ids = app.NewIdentityService(auth.NewDevVerifier(), f.st).WithSAML(NewSAML(), signingVault(t, false), "https://cxthub.example.test")
	active := signingChange(t, f, "activate")
	if active.Connection.Certificate != prepared.Connection.Rotation.Certificate || active.Connection.Rotation.Certificate != original || active.Connection.Rotation.VerifiedAt != nil {
		t.Fatal("bad activation")
	}
	verifyRequestCertificate(t, f, active.Connection.Certificate)
	if _, err = f.ids.ChangeSAMLSigning(ctx, f.user.ID, f.ep.ID, app.SAMLSigningInput{Revision: active.Connection.Revision, Action: "retire"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("untested key retired", err)
	}
	signingRoundtrip(t, f)
	if f.view.Connection.Rotation.VerifiedAt == nil || f.view.VerifiedUntil == nil {
		t.Fatal("roundtrip not recorded")
	}
	updated, err = f.ids.ConfigureSAML(ctx, f.user.ID, f.ep.ID, app.SAMLConnectionInput{Domain: f.domain.Domain, Metadata: f.f.settings.Metadata, Revision: f.view.Connection.Revision})
	if err != nil || updated.Connection.Rotation.VerifiedAt != nil || updated.Connection.Rotation.Certificate != original {
		t.Fatal("provider trust update retained obsolete proof or lost old key", err)
	}
	f.view = updated
	if _, err = f.ids.ChangeSAMLSigning(ctx, f.user.ID, f.ep.ID, app.SAMLSigningInput{Revision: updated.Connection.Revision, Action: "retire"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("old trust verification retired key", err)
	}
	signingRoundtrip(t, f)
	// Restart again, verify persisted state and retire explicitly.
	f.ids = app.NewIdentityService(auth.NewDevVerifier(), f.st).WithSAML(NewSAML(), signingVault(t, false), "https://cxthub.example.test")
	retired := signingChange(t, f, "retire")
	if retired.Connection.Rotation != nil || retired.Connection.Certificate != active.Connection.Certificate {
		t.Fatal("retirement changed active signing")
	}
	md, err = f.ids.SAMLMetadata(ctx, f.ep.ID)
	if err != nil || strings.Count(md, `use="signing"`) != 1 {
		t.Fatal("retired key advertised", err)
	}
	values, err := f.st.ListIdentitySecrets(ctx, f.ep.ID, "", 100)
	if err != nil || len(values) != 1 {
		t.Fatal("retired private key retained", len(values), err)
	}
	events, err := f.ids.ListEnterpriseAudit(ctx, f.user.ID, f.ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, e := range events {
		actions[e.Action] = true
	}
	for _, a := range []string{"prepare", "activate", "retire"} {
		if !actions["enterprise.saml.signing."+a] {
			t.Fatal("missing audit", a)
		}
	}
}
func TestPGSAMLSigningCancelRollbackAndExpiredRecovery(t *testing.T) {
	f := newSAMLFlow(t)
	ctx := context.Background()
	original := f.view.Connection.Certificate
	signingChange(t, f, "prepare")
	signingChange(t, f, "cancel")
	if f.view.Connection.Rotation != nil || f.view.Connection.Certificate != original {
		t.Fatal("cancel lost original")
	}
	signingChange(t, f, "prepare")
	signingChange(t, f, "activate")
	signingChange(t, f, "rollback")
	verifyRequestCertificate(t, f, original)
	// Make only the SP certificate expired. The IdP signing cert remains valid.
	c, err := f.st.GetSAMLConnection(ctx, f.ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	vault, _ := NewVault(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("z", 32))))
	plain, err := vault.Open("saml:"+c.EnterpriseID+":"+c.Revision, c.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := pem.Decode([]byte(plain))
	key, err := x509.ParsePKCS1PrivateKey(kb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := pem.Decode([]byte(c.Certificate))
	cert, _ := x509.ParseCertificate(cb.Bytes)
	cert.NotBefore = time.Now().Add(-48 * time.Hour)
	cert.NotAfter = time.Now().Add(-24 * time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c.Certificate = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if err = f.st.WithinIdentity(ctx, func(ctx context.Context) error { return f.st.PutSAMLConnection(ctx, c) }); err != nil {
		t.Fatal(err)
	}
	if _, err = f.ids.BeginSAML(ctx, f.user.ID, f.ep.ID, f.session.Token); err == nil {
		t.Fatal("expired signer accepted")
	}
	f.view, err = f.ids.ConfigureSAML(ctx, f.user.ID, f.ep.ID, app.SAMLConnectionInput{Domain: f.domain.Domain, Metadata: f.f.settings.Metadata, Revision: c.Revision})
	if err != nil {
		t.Fatal("expired SP certificate prevented IdP trust repair", err)
	}
	signingChange(t, f, "prepare")
	if f.view.SigningCertificate.Status != "expired" {
		t.Fatal("expiry not reported")
	}
	md, err := f.ids.SAMLMetadata(ctx, f.ep.ID)
	if err != nil || strings.Count(md, `use="signing"`) != 2 {
		t.Fatal("expired signer prevents recovery metadata", err)
	}
	signingChange(t, f, "activate")
	if _, err = f.ids.ChangeSAMLSigning(ctx, f.user.ID, f.ep.ID, app.SAMLSigningInput{Revision: f.view.Connection.Revision, Action: "rollback"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("rollback to expired key accepted", err)
	}
	verifyRequestCertificate(t, f, f.view.Connection.Certificate)
	signingRoundtrip(t, f)
	signingChange(t, f, "retire")
}

type signingAuditFailure struct{ *store.PostgresStore }

func (s signingAuditFailure) AppendEnterpriseAudit(context.Context, domain.EnterpriseAuditEvent) error {
	return errors.New("fixture audit failure")
}

func TestPGSAMLSigningAuthorityAuditAndConcurrentActions(t *testing.T) {
	f := newSAMLFlow(t)
	ctx := context.Background()
	other, _, err := f.ids.Login(ctx, "dev:"+domain.NewID("")+"@example.test", "other")
	if err != nil {
		t.Fatal(err)
	}
	in := app.SAMLSigningInput{Revision: f.view.Connection.Revision, Action: "prepare"}
	if _, err = f.ids.ChangeSAMLSigning(ctx, other.ID, f.ep.ID, in); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("non-owner accepted", err)
	}
	before, _ := f.st.GetSAMLConnection(ctx, f.ep.ID)
	vault, _ := NewVault(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("z", 32))))
	bad := app.NewIdentityService(auth.NewDevVerifier(), signingAuditFailure{f.st}).WithSAML(NewSAML(), vault, "https://cxthub.example.test")
	if _, err = bad.ChangeSAMLSigning(ctx, f.user.ID, f.ep.ID, in); err == nil {
		t.Fatal("audit failure ignored")
	}
	after, _ := f.st.GetSAMLConnection(ctx, f.ep.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("audit rollback lost")
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := f.ids.ChangeSAMLSigning(ctx, f.user.ID, f.ep.ID, in); results <- e }()
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for e := range results {
		if e == nil {
			successes++
		} else if errors.Is(e, domain.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal(successes, conflicts)
	}
	f.view, err = f.ids.GetSAMLView(ctx, f.user.ID, f.ep.ID, f.session.Token)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(f.view)
	stored, _ := f.st.GetSAMLConnection(ctx, f.ep.ID)
	if strings.Contains(string(public), stored.PrivateKey) || strings.Contains(string(public), stored.Rotation.PrivateKey) || strings.Contains(string(public), "PrivateKey") {
		t.Fatal("key in public DTO")
	}
	signingChange(t, f, "cancel")
	signingChange(t, f, "prepare")
	signingChange(t, f, "activate")
	state, raw := f.start(t, domain.NewID("audit_"))
	finish, err := f.ids.ReceiveSAML(ctx, f.ep.ID, state, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bad.CompleteSAML(ctx, f.session.Token, finish); err == nil {
		t.Fatal("completion audit failure ignored")
	}
	still, err := f.st.GetSAMLConnection(ctx, f.ep.ID)
	if err != nil || still.Rotation.VerifiedAt != nil {
		t.Fatal("failed callback marked rotation verified", err)
	}
	if _, err = f.ids.CompleteSAML(ctx, f.session.Token, finish); err != nil {
		t.Fatal("rollback consumed callback", err)
	}
	signingChange(t, f, "rollback")
	// Revoke between out-of-lock generation and the transaction authority check.
	if err = f.st.WithinIdentity(ctx, func(ctx context.Context) error {
		return f.st.PutEnterpriseMember(ctx, domain.EnterpriseMembership{EnterpriseID: f.ep.ID, UserID: other.ID, Role: domain.EnterpriseOwner, CreatedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	f.provider.afterKeys = func() {
		err = f.st.WithinIdentity(ctx, func(ctx context.Context) error {
			return f.st.PutEnterpriseMember(ctx, domain.EnterpriseMembership{EnterpriseID: f.ep.ID, UserID: f.user.ID, Role: domain.EnterpriseMember, CreatedAt: time.Now()})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	before, _ = f.st.GetSAMLConnection(ctx, f.ep.ID)
	if _, err = f.ids.ChangeSAMLSigning(ctx, f.user.ID, f.ep.ID, app.SAMLSigningInput{Revision: before.Revision, Action: "prepare"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("revoked owner wrote", err)
	}
	after, _ = f.st.GetSAMLConnection(ctx, f.ep.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("revoked write persisted")
	}
}
