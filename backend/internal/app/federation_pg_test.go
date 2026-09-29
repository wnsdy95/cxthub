//go:build postgres

package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/federation"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type fakeOIDC struct {
	exchanges      atomic.Int32
	beforeExchange func()
	subject        string
	proofTTL       time.Duration
}

func (f *fakeOIDC) Authorize(_ context.Context, c outbound.OIDCSettings, state, nonce, verifier string) (string, error) {
	return c.Issuer + "/authorize?state=" + state, nil
}
func (f *fakeOIDC) Exchange(_ context.Context, c outbound.OIDCSettings, code, nonce, verifier string, started time.Time) (outbound.FederationProof, error) {
	f.exchanges.Add(1)
	if f.beforeExchange != nil {
		f.beforeExchange()
	}
	ttl := f.proofTTL
	if ttl == 0 {
		ttl = time.Hour
	}
	return outbound.FederationProof{Issuer: c.Issuer, Subject: f.subject, AuthenticatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(ttl), AMR: []string{"mfa"}}, nil
}

type federationFixture struct {
	st         *store.PostgresStore
	team       teamFixture
	enterprise domain.Enterprise
	provider   *fakeOIDC
	vault      *federation.Vault
	session    domain.Session
	input      OIDCConnectionInput
}

func newFederationFixture(t *testing.T) federationFixture {
	t.Helper()
	st, f, e := domainPGFixture(t)
	s := f.identity
	ctx := systemTestContext()
	d, err := s.RequestEnterpriseDomain(ctx, f.owner.ID, e.ID, domain.NewID("")+".example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	s.WithDomainResolver(domainTXTFunc(func(context.Context, string) ([]string, error) { return []string{d.Challenge}, nil }))
	if _, err = s.VerifyEnterpriseDomain(ctx, f.owner.ID, e.ID, d.Domain, d.Revision); err != nil {
		t.Fatal(err)
	}
	vault, err := federation.NewVault(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeOIDC{subject: "subject-1"}
	s.WithOIDC(provider, vault, "https://cxthub.example.test")
	sess, err := s.issueSession(ctx, f.owner.ID, "sess_", "web", "fixture", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	in := OIDCConnectionInput{Domain: d.Domain, Issuer: "https://idp.example.test", ClientID: "client", ClientSecret: "synthetic-client-secret", AuthMethod: "client_secret_basic"}
	view, err := s.ConfigureOIDC(ctx, f.owner.ID, e.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Revision = view.Connection.Revision
	return federationFixture{st, f, e, provider, vault, sess, in}
}
func (f federationFixture) start(t *testing.T, token, actor string) string {
	t.Helper()
	auth, err := f.team.identity.BeginOIDC(systemTestContext(), actor, f.enterprise.ID, token)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(auth.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("state")
}
func TestPGOIDCBindsOnlyInitiatingSessionAndConsumesState(t *testing.T) {
	f := newFederationFixture(t)
	s := f.team.identity
	ctx := systemTestContext()
	owner := f.team.owner.ID
	other, err := s.issueSession(ctx, owner, "sess_", "web", "other", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	state := f.start(t, f.session.Token, owner)
	if _, err = s.CompleteOIDC(ctx, other.Token, state, "code"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("wrong browser accepted", err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	resumed := NewIdentityService(nil, peer).WithOIDC(f.provider, f.vault, "https://cxthub.example.test")
	if slug, err := resumed.CompleteOIDC(ctx, f.session.Token, state, "code"); err != nil || slug != f.enterprise.Slug {
		t.Fatal(slug, err)
	}
	if _, err = s.CompleteOIDC(ctx, f.session.Token, state, "code"); err == nil || f.provider.exchanges.Load() != 1 {
		t.Fatal("replayed state", err)
	}
	view, err := s.GetOIDCView(ctx, owner, f.enterprise.ID, f.session.Token)
	if err != nil || !view.Linked || view.VerifiedUntil == nil {
		t.Fatal(view, err)
	}
	otherView, err := s.GetOIDCView(ctx, owner, f.enterprise.ID, other.Token)
	if err != nil || !otherView.Linked || otherView.VerifiedUntil != nil {
		t.Fatal("different credential authorized", err)
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), f.input.ClientSecret) || strings.Contains(string(encoded), `"client_secret":`) || strings.Contains(string(encoded), "subject-1") {
		t.Fatal("private identity leaked", string(encoded))
	}
	c, err := f.st.GetOIDCConnection(ctx, f.enterprise.ID)
	if err != nil || c.Secret == f.input.ClientSecret {
		t.Fatal("credential not encrypted", err)
	}
	a, err := f.st.GetOIDCAttempt(ctx, domain.HashToken(state))
	if err != nil || a.Verifier == "" || !strings.HasPrefix(a.Verifier, "v1.") {
		t.Fatal("PKCE not encrypted", err)
	}
}
func TestPGOIDCRechecksRevocationDuringExchange(t *testing.T) {
	for _, action := range []string{"session", "membership", "connection", "domain", "disable"} {
		t.Run(action, func(t *testing.T) {
			f := newFederationFixture(t)
			s := f.team.identity
			ctx := systemTestContext()
			owner := f.team.owner.ID
			if err := s.UpdateEnterpriseMember(ctx, owner, f.enterprise.ID, f.team.outsider.ID, domain.EnterpriseOwner); err != nil {
				t.Fatal(err)
			}
			state := f.start(t, f.session.Token, owner)
			f.provider.beforeExchange = func() {
				var err error
				switch action {
				case "session":
					err = s.Logout(ctx, f.session.Token)
				case "membership":
					err = s.RemoveEnterpriseMember(ctx, f.team.outsider.ID, f.enterprise.ID, owner)
				case "connection":
					_, err = s.ConfigureOIDC(ctx, owner, f.enterprise.ID, f.input)
				case "disable":
					err = s.DisableOIDC(ctx, owner, f.enterprise.ID, f.input.Revision)
				case "domain":
					d, e := f.st.GetEnterpriseDomain(ctx, f.enterprise.ID, f.input.Domain)
					err = e
					if err == nil {
						err = s.ReleaseEnterpriseDomain(ctx, owner, f.enterprise.ID, d.Domain, d.Revision)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.CompleteOIDC(ctx, f.session.Token, state, "code"); err == nil {
				t.Fatal("revoked state accepted", action)
			}
			if _, err := f.st.GetFederationIdentity(ctx, f.enterprise.ID, "oidc", owner); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("revoked account linked", err)
			}
		})
	}
}
func TestPGOIDCConcurrentCallbacksAndAuditRollback(t *testing.T) {
	for _, failAudit := range []bool{false, true} {
		t.Run(map[bool]string{false: "concurrent", true: "audit_failure"}[failAudit], func(t *testing.T) {
			f := newFederationFixture(t)
			ctx := systemTestContext()
			s := f.team.identity
			state := f.start(t, f.session.Token, f.team.owner.ID)
			if failAudit {
				s = NewIdentityService(nil, domainAuditFailure{f.st}).WithOIDC(f.provider, f.vault, "https://cxthub.example.test")
			}
			results := make(chan error, 2)
			for range 2 {
				go func() { _, err := s.CompleteOIDC(ctx, f.session.Token, state, "code"); results <- err }()
			}
			success := 0
			for range 2 {
				if <-results == nil {
					success++
				}
			}
			want := 1
			if failAudit {
				want = 0
			}
			if success != want || f.provider.exchanges.Load() != 1 {
				t.Fatal("callback replay or incorrect atomicity", success, f.provider.exchanges.Load())
			}
			if failAudit {
				if _, err := f.st.GetFederationIdentity(ctx, f.enterprise.ID, "oidc", f.team.owner.ID); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("identity escaped rollback", err)
				}
				if _, err := f.st.GetFederationSession(ctx, f.enterprise.ID, domain.HashToken(f.session.Token)); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("proof escaped rollback", err)
				}
			}
		})
	}
}
func TestPGOIDCNeverLinksOneExternalSubjectToTwoUsers(t *testing.T) {
	f := newFederationFixture(t)
	s := f.team.identity
	ctx := systemTestContext()
	state := f.start(t, f.session.Token, f.team.owner.ID)
	if _, err := s.CompleteOIDC(ctx, f.session.Token, state, "code"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateEnterpriseMember(ctx, f.team.owner.ID, f.enterprise.ID, f.team.member.ID, domain.EnterpriseMember); err != nil {
		t.Fatal(err)
	}
	sess, err := s.issueSession(ctx, f.team.member.ID, "sess_", "web", "member", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	state = f.start(t, sess.Token, f.team.member.ID)
	if _, err = s.CompleteOIDC(ctx, sess.Token, state, "code"); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("external identity reassigned", err)
	}
	if _, err = f.st.GetFederationIdentity(ctx, f.enterprise.ID, "oidc", f.team.member.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("duplicate identity", err)
	}
}

func TestPGOIDCRequiresRecentInitialLoginAndCurrentAttempt(t *testing.T) {
	f := newFederationFixture(t)
	s := f.team.identity
	ctx := systemTestContext()
	state := f.start(t, f.session.Token, f.team.owner.ID)
	conn, err := pgxpool.New(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Change only this synthetic test credential. Session creation intentionally
	// uses the database clock in production.
	if _, err = conn.Exec(ctx, `UPDATE sessions SET created_at=now()-interval '11 minutes' WHERE token=$1`, domain.HashToken(f.session.Token)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BeginOIDC(ctx, f.team.owner.ID, f.enterprise.ID, f.session.Token); !errors.Is(err, domain.ErrRecentIdentityLogin) {
		t.Fatal("old login linked", err)
	}
	if _, err = s.CompleteOIDC(ctx, f.session.Token, state, "code"); !errors.Is(err, domain.ErrRecentIdentityLogin) || f.provider.exchanges.Load() != 0 {
		t.Fatal("old login callback", err)
	}
	if _, err = conn.Exec(ctx, `UPDATE sessions SET created_at=now() WHERE token=$1`, domain.HashToken(f.session.Token)); err != nil {
		t.Fatal(err)
	}
	a, err := f.st.GetOIDCAttempt(ctx, domain.HashToken(state))
	if err != nil {
		t.Fatal(err)
	}
	a.ExpiresAt = time.Now().Add(-time.Minute)
	raw, _ := json.Marshal(a)
	if _, err = conn.Exec(ctx, `UPDATE enterprise_oidc_attempts SET record=$2 WHERE hash=$1`, a.Hash, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteOIDC(ctx, f.session.Token, state, "code"); !errors.Is(err, domain.ErrConflict) || f.provider.exchanges.Load() != 0 {
		t.Fatal("expired attempt exchanged code", err)
	}
}

func TestPGOIDCConfigurationAuditRollback(t *testing.T) {
	f := newFederationFixture(t)
	ctx := systemTestContext()
	s := NewIdentityService(nil, domainAuditFailure{f.st}).WithOIDC(f.provider, f.vault, "https://cxthub.example.test")
	if _, err := s.ConfigureOIDC(ctx, f.team.owner.ID, f.enterprise.ID, f.input); err == nil {
		t.Fatal("configuration without audit")
	}
	if err := s.DisableOIDC(ctx, f.team.owner.ID, f.enterprise.ID, f.input.Revision); err == nil {
		t.Fatal("disable without audit")
	}
	current, err := f.st.GetOIDCConnection(ctx, f.enterprise.ID)
	if err != nil || current.Revision != f.input.Revision {
		t.Fatal("configuration escaped rollback", err)
	}
}
