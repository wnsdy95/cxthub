//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func verifiedAssuranceFixture(t *testing.T) (federationFixture, domain.Session, domain.OAuthTokenPair, CredentialAssurancesView) {
	t.Helper()
	f := newFederationFixture(t)
	ctx := systemTestContext()
	s := f.team.identity
	actor := f.team.owner.ID
	cli, err := s.issueSession(ctx, actor, "sess_cli_", "cli", "test terminal", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mcp, err := s.IssueMCPTokenPair(ctx, actor, "test-mcp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteOIDC(ctx, f.session.Token, f.start(t, f.session.Token, actor), "code"); err != nil {
		t.Fatal(err)
	}
	v, err := s.GetCredentialAssurances(ctx, actor, f.enterprise.ID, f.session.Token)
	if err != nil || v.BrowserProof == nil || len(v.Credentials) != 2 {
		t.Fatal("inventory", v, err)
	}
	return f, cli, mcp, v
}
func assuranceTarget(t *testing.T, v CredentialAssurancesView, kind string) CredentialAssuranceView {
	t.Helper()
	for _, c := range v.Credentials {
		if c.Kind == kind {
			return c
		}
	}
	t.Fatal("target missing", kind)
	return CredentialAssuranceView{}
}
func assertAssuranceState(t *testing.T, s *IdentityService, f federationFixture, id, state string) CredentialAssuranceView {
	t.Helper()
	v, err := s.GetCredentialAssurances(systemTestContext(), f.team.owner.ID, f.enterprise.ID, f.session.Token)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Credentials {
		if c.ID == id {
			if c.State != state {
				t.Fatal("state", c.State, "want", state)
			}
			return c
		}
	}
	t.Fatal("target disappeared", id)
	return CredentialAssuranceView{}
}
func TestPGCredentialAssuranceIsolationRefreshAndRevocation(t *testing.T) {
	f, cli, mcp, v := verifiedAssuranceFixture(t)
	s := f.team.identity
	ctx := systemTestContext()
	actor := f.team.owner.ID
	ep := f.enterprise.ID
	mc := assuranceTarget(t, v, "mcp")
	cc := assuranceTarget(t, v, "cli")
	if err := s.withIdentity(ctx, func(ctx context.Context) error {
		g, err := f.st.GetOAuthGrant(ctx, mc.ID)
		if err != nil {
			return err
		}
		g.ExpiresAt = time.Now().Add(5 * time.Minute)
		return f.st.UpdateOAuthGrant(ctx, g)
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v)
	for _, secret := range []string{cli.Token, domain.HashToken(cli.Token), mcp.AccessToken, domain.HashToken(mcp.AccessToken), "subject-1"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("inventory leaked private data")
		}
	}
	if err := s.ApproveCredential(ctx, actor, ep, f.session.Token, mc.ID, *v.BrowserProof); err != nil {
		t.Fatal(err)
	}
	before := assertAssuranceState(t, s, f, mc.ID, "approved")
	assertAssuranceState(t, s, f, cc.ID, "unapproved")
	next, err := s.RefreshMCPAccessToken(ctx, mcp.RefreshToken, "test-mcp")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveCredential(ctx, actor, ep, f.session.Token, mc.ID, *v.BrowserProof); err != nil {
		t.Fatal(err)
	}
	after := assertAssuranceState(t, s, f, mc.ID, "approved")
	if !before.VerifiedUntil.Equal(*after.VerifiedUntil) || !before.AuthenticatedAt.Equal(*after.AuthenticatedAt) {
		t.Fatal("refresh extended authentication")
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	assertAssuranceState(t, NewIdentityService(nil, peer), f, mc.ID, "approved")
	if err = s.RevokeCredentialAssurance(ctx, actor, ep, f.session.Token, mc.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeCredentialAssurance(ctx, actor, ep, f.session.Token, mc.ID); err != nil {
		t.Fatal(err)
	}
	assertAssuranceState(t, s, f, mc.ID, "revoked")
	if _, err = s.ResolveMCPUser(ctx, next.AccessToken); err != nil {
		t.Fatal("approval revoke deleted credential", err)
	}
	if err = s.ApproveCredential(ctx, actor, ep, f.session.Token, cc.ID, *v.BrowserProof); err != nil {
		t.Fatal(err)
	}
	assertAssuranceState(t, s, f, cc.ID, "approved")
	// Explicit credential deletion immediately removes it from the live inventory.
	if err = s.withIdentity(ctx, func(ctx context.Context) error { return f.st.DeleteSession(ctx, domain.HashToken(cli.Token)) }); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetCredentialAssurances(ctx, actor, ep, f.session.Token)
	if err != nil || len(current.Credentials) != 1 {
		t.Fatal(current, err)
	}
	audit, err := f.st.ListEnterpriseAudit(ctx, ep, 100)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, a := range audit {
		if a.Action == "enterprise.credential.revoked" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("revoke audit", count)
	}
}
func TestPGCredentialAssuranceRejectsWrongCredentialAndStaleProof(t *testing.T) {
	f, cli, _, v := verifiedAssuranceFixture(t)
	s := f.team.identity
	ctx := systemTestContext()
	actor := f.team.owner.ID
	ep := f.enterprise.ID
	c := assuranceTarget(t, v, "cli")
	other, err := s.issueSession(ctx, actor, "sess_", "web", "peer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApproveCredential(ctx, actor, ep, other.Token, c.ID, *v.BrowserProof); !errors.Is(err, domain.ErrRecentIdentityLogin) {
		t.Fatal("peer browser", err)
	}
	if err = s.ApproveCredential(ctx, actor, ep, cli.Token, c.ID, *v.BrowserProof); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("machine approval", err)
	}
	if _, err = s.GetCredentialAssurances(ctx, actor, ep, cli.Token); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("machine inventory", err)
	}
	bad := *v.BrowserProof
	bad.ConnectionRevision = "old"
	if err = s.ApproveCredential(ctx, actor, ep, f.session.Token, c.ID, bad); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale", err)
	}
	foreign, err := s.issueSession(ctx, f.team.outsider.ID, "sess_cli_", "cli", "foreign", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	record, err := f.st.GetSession(ctx, domain.HashToken(foreign.Token))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{record.CredentialID, domain.TokenHint(cli.Token)} {
		if err = s.ApproveCredential(ctx, actor, ep, f.session.Token, target, *v.BrowserProof); err == nil {
			t.Fatal("foreign/hint approval accepted")
		}
	}
	// An old but still valid browser proof may view status; it cannot approve.
	err = s.withIdentity(ctx, func(ctx context.Context) error {
		p, e := f.st.GetFederationSession(ctx, ep, domain.HashToken(f.session.Token))
		if e != nil {
			return e
		}
		p.AuthenticatedAt = time.Now().Add(-11 * time.Minute)
		return f.st.PutFederationSession(ctx, p)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApproveCredential(ctx, actor, ep, f.session.Token, c.ID, *v.BrowserProof); !errors.Is(err, domain.ErrRecentIdentityLogin) {
		t.Fatal("stale auth", err)
	}
	assertAssuranceState(t, s, f, c.ID, "unapproved")
}
func TestPGCredentialAssuranceInvalidation(t *testing.T) {
	for _, change := range []string{"policy", "connection", "domain", "membership", "expiry", "legacy"} {
		t.Run(change, func(t *testing.T) {
			f, _, _, v := verifiedAssuranceFixture(t)
			s := f.team.identity
			ctx := systemTestContext()
			actor := f.team.owner.ID
			ep := f.enterprise.ID
			c := assuranceTarget(t, v, "cli")
			if err := s.ApproveCredential(ctx, actor, ep, f.session.Token, c.ID, *v.BrowserProof); err != nil {
				t.Fatal(err)
			}
			var err error
			switch change {
			case "policy":
				err = s.ConfigureAssurancePolicy(ctx, actor, ep, f.session.Token, AssurancePolicyInput{v.Policy.Revision, 1})
			case "connection":
				_, err = s.ConfigureOIDC(ctx, actor, ep, f.input)
			case "domain":
				d, e := f.st.GetEnterpriseDomain(ctx, ep, f.input.Domain)
				if e != nil {
					t.Fatal(e)
				}
				err = s.ReleaseEnterpriseDomain(ctx, actor, ep, d.Domain, d.Revision)
			case "membership":
				if err = s.UpdateEnterpriseMember(ctx, actor, ep, f.team.outsider.ID, domain.EnterpriseOwner); err != nil {
					t.Fatal(err)
				}
				err = s.RemoveEnterpriseMember(ctx, f.team.outsider.ID, ep, actor)
			case "expiry", "legacy":
				err = s.withIdentity(ctx, func(ctx context.Context) error {
					all, e := f.st.ListCredentialAssurances(ctx, ep, actor)
					if e != nil {
						return e
					}
					a := all[0]
					if change == "expiry" {
						a.ApprovedAt = time.Now().Add(-2 * time.Hour)
						a.Proof.ExpiresAt = time.Now().Add(-time.Hour)
					} else {
						p, e := f.st.GetFederationSession(ctx, ep, domain.HashToken(f.session.Token))
						if e != nil {
							return e
						}
						p.PolicyRevision = ""
						return f.st.PutFederationSession(ctx, p)
					}
					return f.st.PutCredentialAssurance(ctx, a)
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			view, err := s.GetCredentialAssurances(ctx, actor, ep, f.session.Token)
			if change == "membership" {
				if !errors.Is(err, domain.ErrForbidden) {
					t.Fatal("membership not checked", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := "verification_changed"
			if change == "expiry" {
				want = "expired"
			}
			if change == "legacy" {
				want = "approved"
				if view.BrowserProof != nil {
					t.Fatal("legacy proof upgraded")
				}
			}
			if assuranceTarget(t, view, "cli").State != want {
				t.Fatal(view)
			}
			if change != "expiry" {
				if err = s.ApproveCredential(ctx, actor, ep, f.session.Token, c.ID, *v.BrowserProof); err == nil {
					t.Fatal("invalidated proof approved")
				}
			}
		})
	}
}
func TestPGCredentialAssuranceAuditRollback(t *testing.T) {
	f, _, _, v := verifiedAssuranceFixture(t)
	ctx := systemTestContext()
	actor := f.team.owner.ID
	ep := f.enterprise.ID
	c := assuranceTarget(t, v, "cli")
	broken := NewIdentityService(nil, domainAuditFailure{f.st})
	if err := broken.ApproveCredential(ctx, actor, ep, f.session.Token, c.ID, *v.BrowserProof); err == nil {
		t.Fatal("audit failure accepted")
	}
	assertAssuranceState(t, f.team.identity, f, c.ID, "unapproved")
	if err := f.team.identity.ApproveCredential(ctx, actor, ep, f.session.Token, c.ID, *v.BrowserProof); err != nil {
		t.Fatal(err)
	}
	if err := broken.RevokeCredentialAssurance(ctx, actor, ep, f.session.Token, c.ID); err == nil {
		t.Fatal("audit failure accepted")
	}
	assertAssuranceState(t, f.team.identity, f, c.ID, "approved")
	if err := broken.ConfigureAssurancePolicy(ctx, actor, ep, f.session.Token, AssurancePolicyInput{v.Policy.Revision, 1}); err == nil {
		t.Fatal("policy audit failure accepted")
	}
	assertAssuranceState(t, f.team.identity, f, c.ID, "approved")
}
