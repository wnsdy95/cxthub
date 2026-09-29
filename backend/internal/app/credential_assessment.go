package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// CredentialAssessment is evidence, not an access grant. Authorization must
// also check the action's role and an explicitly enabled access policy.
// No ACR/AMR claim is silently interpreted as MFA.
type CredentialAssessment struct {
	State           string     `json:"state"`
	Protocol        string     `json:"protocol,omitempty"`
	AuthenticatedAt *time.Time `json:"authenticated_at,omitempty"`
	VerifiedUntil   *time.Time `json:"verified_until,omitempty"`
}

func (s *IdentityService) assessmentProof(ctx context.Context, st outbound.CredentialAssurances, p domain.FederationSession, revoked bool, now time.Time) (CredentialAssessment, error) {
	out := CredentialAssessment{State: "verification_changed", Protocol: p.Protocol, AuthenticatedAt: &p.AuthenticatedAt, VerifiedUntil: &p.ExpiresAt}
	if revoked {
		out.State = "revoked"
		return out, nil
	}
	if !now.Before(p.ExpiresAt) {
		out.State = "expired"
		return out, nil
	}
	valid, err := s.federationProofCurrent(ctx, st, p, now)
	if err != nil {
		return CredentialAssessment{}, err
	}
	if valid {
		out.State = "verified"
	}
	return out, nil
}

func (s *IdentityService) assessEnterpriseCredential(ctx context.Context, actor, ep, token string) (CredentialAssessment, error) {
	out := CredentialAssessment{State: "unverified"}
	if !s.canReadEnterprise(ctx, ep, actor) {
		return out, domain.ErrForbidden
	}
	if !strings.HasPrefix(token, "sess_") {
		return out, domain.ErrUnauthorized
	}
	st, err := s.assuranceStore()
	if err != nil {
		return out, err
	}
	// Logout/revocation must serialize with an assessment used by a protected
	// write in this transaction, including stores where logout is a row delete.
	sess, err := st.LockFederationSession(ctx, domain.HashToken(token))
	now := time.Now().UTC()
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return out, domain.ErrUnauthorized
		}
		return out, err
	}
	if sess.UserID != actor || !now.Before(sess.ExpiresAt) {
		return out, domain.ErrUnauthorized
	}
	var proof domain.FederationSession
	switch sess.Kind {
	case "web":
		proof, err = st.GetFederationSession(ctx, ep, sess.Token)
		if errors.Is(err, domain.ErrNotFound) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		if proof.SessionHash != sess.Token || proof.UserID != actor || proof.EnterpriseID != ep {
			return out, domain.ErrIntegrity
		}
	case "cli", "mcp_access":
		target := sess.CredentialID
		if sess.Kind == "mcp_access" {
			if err = s.checkMCPGrant(ctx, sess); err != nil {
				return out, err
			}
			target = sess.GrantID
		}
		if target == "" {
			return out, nil
		}
		records, err := st.ListCredentialAssurances(ctx, ep, actor)
		if err != nil {
			return out, err
		}
		found := false
		for _, r := range records {
			if r.CredentialID == target {
				if r.RevokedAt != nil {
					return s.assessmentProof(ctx, st, r.Proof, true, now)
				}
				proof = r.Proof
				found = true
				break
			}
		}
		if !found {
			return out, nil
		}
	default:
		return out, domain.ErrUnauthorized
	}
	out, err = s.assessmentProof(ctx, st, proof, false, now)
	if err == nil && out.VerifiedUntil != nil && sess.ExpiresAt.Before(*out.VerifiedUntil) {
		out.VerifiedUntil = &sess.ExpiresAt
	}
	return out, err
}

// The same internal evaluator can run inside a protected write transaction.
// Standalone callers get one coherent identity observation, across API replicas.
func (s *IdentityService) AssessEnterpriseCredential(ctx context.Context, actor, ep, token string) (CredentialAssessment, error) {
	return identityResult(ctx, s, func(ctx context.Context) (CredentialAssessment, error) {
		return s.assessEnterpriseCredential(ctx, actor, ep, token)
	})
}
