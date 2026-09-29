//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGAssuranceApprovalRacesRevocationAndPolicy(t *testing.T) {
	for _, action := range []string{"cli", "mcp", "policy", "connection", "membership"} {
		t.Run(action, func(t *testing.T) {
			f, cli, mcp, v := verifiedAssuranceFixture(t)
			ctx := systemTestContext()
			s := f.team.identity
			actor := f.team.owner.ID
			ep := f.enterprise.ID
			peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			other := NewIdentityService(nil, peer).WithOIDC(f.provider, f.vault, "https://cxthub.example.test")
			if action == "membership" {
				if err := s.UpdateEnterpriseMember(ctx, actor, ep, f.team.outsider.ID, domain.EnterpriseOwner); err != nil {
					t.Fatal(err)
				}
			}
			kind := "cli"
			if action == "mcp" {
				kind = "mcp"
			}
			c := assuranceTarget(t, v, kind)
			start := make(chan struct{})
			approved := make(chan error, 1)
			changed := make(chan error, 1)
			go func() {
				<-start
				approved <- s.ApproveCredential(ctx, actor, ep, f.session.Token, c.ID, *v.BrowserProof)
			}()
			go func() {
				<-start
				var e error
				switch action {
				case "cli":
					e = peer.DeleteSession(ctx, domain.HashToken(cli.Token))
				case "mcp":
					e = other.RevokeMCPToken(ctx, mcp.RefreshToken, "test-mcp")
				case "policy":
					e = other.ConfigureAssurancePolicy(ctx, actor, ep, f.session.Token, AssurancePolicyInput{v.Policy.Revision, 1})
				case "connection":
					_, e = other.ConfigureOIDC(ctx, actor, ep, f.input)
				case "membership":
					e = other.RemoveEnterpriseMember(ctx, f.team.outsider.ID, ep, actor)
				}
				changed <- e
			}()
			close(start)
			if err = <-changed; err != nil {
				t.Fatal(err)
			}
			err = <-approved
			if err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrRecentIdentityLogin) && !errors.Is(err, domain.ErrForbidden) {
				t.Fatal(err)
			}
			current, err := other.GetCredentialAssurances(ctx, actor, ep, f.session.Token)
			if action == "membership" {
				if !errors.Is(err, domain.ErrForbidden) {
					t.Fatal("removed member retained inventory", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range current.Credentials {
				if item.ID == c.ID && (action == "cli" || action == "mcp" || item.State == "approved") {
					t.Fatal("revoked credential/policy stayed approved", item)
				}
			}
		})
	}
}

func TestPGAssuranceSeparatesOIDCEnvelopeAndAuthentication(t *testing.T) {
	f := newFederationFixture(t)
	s := f.team.identity
	ctx := systemTestContext()
	f.provider.proofTTL = 30 * time.Second
	if _, err := s.CompleteOIDC(ctx, f.session.Token, f.start(t, f.session.Token, f.team.owner.ID), "code"); err != nil {
		t.Fatal(err)
	}
	proof, err := f.st.GetFederationSession(ctx, f.enterprise.ID, domain.HashToken(f.session.Token))
	if err != nil {
		t.Fatal(err)
	}
	if !proof.ExpiresAt.After(proof.ProofExpiresAt.Add(30 * time.Minute)) {
		t.Fatal("ID token expiry became assurance lifetime")
	}
	if proof.ExpiresAt.After(f.session.ExpiresAt) {
		t.Fatal("browser expiry ignored")
	}
	// The consumed envelope's deadline does not invalidate recorded evidence.
	err = s.withIdentity(ctx, func(ctx context.Context) error {
		proof.ProofExpiresAt = time.Now().Add(-time.Second)
		return f.st.PutFederationSession(ctx, proof)
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.GetCredentialAssurances(ctx, f.team.owner.ID, f.enterprise.ID, f.session.Token)
	if err != nil || v.BrowserProof == nil {
		t.Fatal(v, err)
	}
	if err = s.ConfigureAssurancePolicy(ctx, f.team.owner.ID, f.enterprise.ID, f.session.Token, AssurancePolicyInput{v.Policy.Revision, 24}); err != nil {
		t.Fatal(err)
	}
	v, err = s.GetCredentialAssurances(ctx, f.team.owner.ID, f.enterprise.ID, f.session.Token)
	if err != nil || v.BrowserProof != nil {
		t.Fatal("policy extended existing proof", v, err)
	}
}
