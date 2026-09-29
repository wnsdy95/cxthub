//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGCredentialAssessmentExactLifecycle(t *testing.T) {
	f, cli, mcp, v := verifiedAssuranceFixture(t)
	s, ctx, ep, actor := f.team.identity, systemTestContext(), f.enterprise.ID, f.team.owner.ID
	check := func(token, state string) {
		t.Helper()
		got, err := s.AssessEnterpriseCredential(ctx, actor, ep, token)
		if err != nil || got.State != state {
			t.Fatal(got, err, "want", state)
		}
	}
	check(f.session.Token, "verified")
	check(cli.Token, "unverified")
	check(mcp.AccessToken, "unverified")
	other, err := s.issueSession(ctx, actor, "sess_", "web", "other", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	check(other.Token, "unverified")
	for _, kind := range []string{"cli", "mcp"} {
		target := assuranceTarget(t, v, kind)
		if err := s.ApproveCredential(ctx, actor, ep, f.session.Token, target.ID, *v.BrowserProof); err != nil {
			t.Fatal(err)
		}
	}
	check(cli.Token, "verified")
	check(mcp.AccessToken, "verified")
	rotated, err := s.RefreshMCPAccessToken(ctx, mcp.RefreshToken, "test-mcp")
	if err != nil {
		t.Fatal(err)
	}
	check(rotated.AccessToken, "verified")
	for _, invalid := range []string{rotated.RefreshToken, "dev:owner@example.test", "sess_missing"} {
		if _, err := s.AssessEnterpriseCredential(ctx, actor, ep, invalid); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
	if _, err := s.AssessEnterpriseCredential(ctx, f.team.outsider.ID, ep, cli.Token); err == nil {
		t.Fatal("wrong user assessed")
	}
	if err := s.RevokeCredentialAssurance(ctx, actor, ep, f.session.Token, assuranceTarget(t, v, "cli").ID); err != nil {
		t.Fatal(err)
	}
	check(cli.Token, "revoked")
	if err := s.ConfigureAssurancePolicy(ctx, actor, ep, f.session.Token, AssurancePolicyInput{v.Policy.Revision, 1}); err != nil {
		t.Fatal(err)
	}
	check(f.session.Token, "verification_changed")
	check(rotated.AccessToken, "verification_changed")
	if err := s.RevokeMCPToken(ctx, rotated.RefreshToken, "test-mcp"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AssessEnterpriseCredential(ctx, actor, ep, rotated.AccessToken); err == nil {
		t.Fatal("revoked grant assessed")
	}
}

func TestPGOwnerRecoveryInvalidationAndStaleEditors(t *testing.T) {
	for _, change := range []string{"demote", "remove", "logout", "expiry", "revoke", "rotation"} {
		t.Run(change, func(t *testing.T) {
			f, _, _, _ := verifiedAssuranceFixture(t)
			s, ctx, ep, actor := f.team.identity, systemTestContext(), f.enterprise.ID, f.team.owner.ID
			if err := s.UpdateEnterpriseMember(ctx, actor, ep, f.team.outsider.ID, domain.EnterpriseOwner); err != nil {
				t.Fatal(err)
			}
			v, _ := s.GetOwnerRecovery(ctx, actor, ep, f.session.Token)
			p, err := s.PrepareOwnerRecovery(ctx, actor, ep, f.session.Token, v.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.PrepareOwnerRecovery(ctx, actor, ep, f.session.Token, v.Revision); !errors.Is(err, domain.ErrConflict) {
				t.Fatal("stale prepare", err)
			}
			if change == "expiry" {
				err = s.withIdentity(ctx, func(ctx context.Context) error {
					r, e := f.st.GetOwnerRecovery(ctx, ep, actor)
					if e != nil {
						return e
					}
					past := time.Now().Add(-time.Second)
					r.PendingUntil = &past
					return f.st.PutOwnerRecovery(ctx, r)
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := s.ConfirmOwnerRecovery(ctx, actor, ep, f.session.Token, p.Revision, p.Code); !errors.Is(err, domain.ErrUnauthorized) {
					t.Fatal("expired confirmation", err)
				}
				return
			}
			if err := s.ConfirmOwnerRecovery(ctx, actor, ep, f.session.Token, p.Revision, p.Code); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "demote":
				err = s.UpdateEnterpriseMember(ctx, f.team.outsider.ID, ep, actor, domain.EnterpriseMember)
			case "remove":
				err = s.RemoveEnterpriseMember(ctx, f.team.outsider.ID, ep, actor)
			case "logout":
				err = s.Logout(ctx, f.session.Token)
			case "revoke":
				v, _ = s.GetOwnerRecovery(ctx, actor, ep, f.session.Token)
				err = s.RevokeOwnerRecovery(ctx, actor, ep, f.session.Token, v.Revision)
			case "rotation":
				v, _ = s.GetOwnerRecovery(ctx, actor, ep, f.session.Token)
				var next PreparedOwnerRecovery
				next, err = s.PrepareOwnerRecovery(ctx, actor, ep, f.session.Token, v.Revision)
				if err == nil {
					err = s.ConfirmOwnerRecovery(ctx, actor, ep, f.session.Token, next.Revision, next.Code)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.RedeemOwnerRecovery(ctx, actor, ep, f.session.Token, p.Code); err == nil {
				t.Fatal("stale recovery accepted", change)
			}
			if change == "demote" || change == "remove" {
				if err := s.UpdateEnterpriseMember(ctx, f.team.outsider.ID, ep, actor, domain.EnterpriseOwner); err != nil {
					t.Fatal(err)
				}
				if _, err := s.RedeemOwnerRecovery(ctx, actor, ep, f.session.Token, p.Code); err == nil {
					t.Fatal("re-promoting revived code")
				}
			}
		})
	}
}
