package app

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type CredentialApprovalInput struct {
	Protocol           string `json:"protocol"`
	ConnectionRevision string `json:"connection_revision"`
	PolicyRevision     string `json:"policy_revision"`
}
type AssurancePolicyInput struct {
	Revision    string `json:"revision"`
	MaxAgeHours int    `json:"max_age_hours"`
}
type CredentialAssuranceView struct {
	ID              string     `json:"id"`
	Kind            string     `json:"kind"`
	Label           string     `json:"label"`
	Hint            string     `json:"hint,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
	State           string     `json:"state"`
	Protocol        string     `json:"protocol,omitempty"`
	AuthenticatedAt *time.Time `json:"authenticated_at,omitempty"`
	VerifiedUntil   *time.Time `json:"verified_until,omitempty"`
}
type CredentialAssurancesView struct {
	Available              bool                      `json:"available"`
	Policy                 *domain.AssurancePolicy   `json:"policy,omitempty"`
	BrowserProof           *CredentialApprovalInput  `json:"browser_proof,omitempty"`
	ApprovalAvailableUntil *time.Time                `json:"approval_available_until,omitempty"`
	Credentials            []CredentialAssuranceView `json:"credentials"`
}

func (s *IdentityService) assuranceStore() (outbound.CredentialAssurances, error) {
	st, ok := s.repositories.(outbound.CredentialAssurances)
	if !ok {
		return nil, domain.ErrFederationUnavailable
	}
	return st, nil
}

// makeFederationSession runs only after protocol validation and under the
// identity transaction. Existing evidence is never upgraded to a longer life.
func (s *IdentityService) makeFederationSession(ctx context.Context, ep, protocol, revision, name string, sess domain.Session, proof outbound.FederationProof) (domain.FederationSession, error) {
	var out domain.FederationSession
	st, err := s.assuranceStore()
	if err != nil {
		return out, err
	}
	policy, err := st.GetAssurancePolicy(ctx, ep)
	if err != nil {
		return out, err
	}
	ds, ok := s.repositories.(outbound.EnterpriseDomainStore)
	if !ok {
		return out, domain.ErrFederationUnavailable
	}
	d, err := ds.GetEnterpriseDomain(ctx, ep, name)
	if err != nil {
		return out, err
	}
	now := time.Now().UTC()
	if !d.Verified(now) || !now.Before(proof.ExpiresAt) {
		return out, domain.ErrUnauthorized
	}
	expires, err := policy.AssuranceDeadline(proof.AuthenticatedAt, sess.ExpiresAt, now, proof.SessionExpiresAt)
	if err != nil {
		return out, err
	}
	if d.VerifiedUntil.Before(expires) {
		expires = d.VerifiedUntil
	}
	return domain.FederationSession{EnterpriseID: ep, Protocol: protocol, SessionHash: sess.Token, ConnectionRevision: revision, UserID: sess.UserID, Issuer: proof.Issuer, Subject: proof.Subject, ACR: proof.ACR, AMR: proof.AMR, AuthenticatedAt: proof.AuthenticatedAt, ExpiresAt: expires, ProofExpiresAt: proof.ExpiresAt, IdPSessionExpiresAt: proof.SessionExpiresAt, PolicyRevision: policy.Revision, DomainRevision: d.Revision}, nil
}

// federationProofCurrent is shared by browser and machine read models. Missing
// policy/domain provenance (legacy evidence) requires a fresh verification.
func (s *IdentityService) federationProofCurrent(ctx context.Context, st outbound.CredentialAssurances, p domain.FederationSession, now time.Time) (bool, error) {
	if p.PolicyRevision == "" || p.DomainRevision == "" || p.Subject == "" || p.AuthenticatedAt.IsZero() || !now.Before(p.ExpiresAt) || !s.canReadEnterprise(ctx, p.EnterpriseID, p.UserID) {
		return false, nil
	}
	policy, err := st.GetAssurancePolicy(ctx, p.EnterpriseID)
	if err != nil {
		return false, err
	}
	if policy.Revision != p.PolicyRevision || now.Sub(p.AuthenticatedAt) >= time.Duration(policy.MaxAgeHours)*time.Hour || p.IdPSessionExpiresAt != nil && !now.Before(*p.IdPSessionExpiresAt) {
		return false, nil
	}
	var revision, issuer, name string
	switch p.Protocol {
	case "oidc":
		c, e := st.GetOIDCConnection(ctx, p.EnterpriseID)
		err = e
		revision, issuer, name = c.Revision, c.Issuer, c.Domain
	case "saml":
		ss, ok := s.repositories.(outbound.SAMLStore)
		if !ok {
			return false, domain.ErrFederationUnavailable
		}
		c, e := ss.GetSAMLConnection(ctx, p.EnterpriseID)
		err = e
		revision, issuer, name = c.Revision, c.Issuer, c.Domain
	default:
		return false, nil
	}
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if revision != p.ConnectionRevision || issuer != p.Issuer {
		return false, nil
	}
	identity, err := st.GetFederationIdentity(ctx, p.EnterpriseID, p.Protocol, p.UserID)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if identity.Issuer != p.Issuer || identity.Subject != p.Subject {
		return false, nil
	}
	ds, ok := s.repositories.(outbound.EnterpriseDomainStore)
	if !ok {
		return false, domain.ErrFederationUnavailable
	}
	d, err := ds.GetEnterpriseDomain(ctx, p.EnterpriseID, name)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return d.Verified(now) && d.Revision == p.DomainRevision, nil
}

func (s *IdentityService) assuranceBrowser(ctx context.Context, st outbound.CredentialAssurances, actor, ep, token string) (domain.Session, error) {
	if !strings.HasPrefix(token, "sess_") {
		return domain.Session{}, domain.ErrUnauthorized
	}
	sess, err := st.LockFederationSession(ctx, domain.HashToken(token))
	if err != nil || sess.Kind != "web" || sess.UserID != actor || !time.Now().Before(sess.ExpiresAt) {
		return domain.Session{}, domain.ErrUnauthorized
	}
	if !s.canReadEnterprise(ctx, ep, actor) {
		return domain.Session{}, domain.ErrForbidden
	}
	return sess, nil
}

func (s *IdentityService) assuranceTargets(ctx context.Context, actor string) ([]CredentialAssuranceView, error) {
	sessions, err := s.repositories.ListSessionsForUser(ctx, actor)
	if err != nil {
		return nil, err
	}
	out := []CredentialAssuranceView{}
	seen := map[string]bool{}
	now := time.Now()
	for _, sess := range sessions {
		if !now.Before(sess.ExpiresAt) {
			continue
		}
		if sessionKind(sess) == "cli" && sess.CredentialID != "" {
			out = append(out, CredentialAssuranceView{ID: sess.CredentialID, Kind: "cli", Label: sess.Label, Hint: sessionHint(sess), CreatedAt: sess.CreatedAt, ExpiresAt: sess.ExpiresAt, State: "unapproved"})
		} else if (sess.Kind == "mcp_access" || sess.Kind == "mcp_refresh") && sess.GrantID != "" && !seen[sess.GrantID] {
			seen[sess.GrantID] = true
			gs, ok := s.repositories.(outbound.OAuthGrants)
			if !ok {
				return nil, domain.ErrFederationUnavailable
			}
			g, err := gs.GetOAuthGrant(ctx, sess.GrantID)
			if err != nil {
				return nil, err
			}
			if g.UserID != actor || g.ClientID != sess.Label {
				return nil, domain.ErrIntegrity
			}
			if g.Active(now) {
				out = append(out, CredentialAssuranceView{ID: g.ID, Kind: "mcp", Label: g.ClientID, CreatedAt: g.CreatedAt, ExpiresAt: g.ExpiresAt, State: "unapproved"})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (s *IdentityService) GetCredentialAssurances(ctx context.Context, actor, ep, token string) (CredentialAssurancesView, error) {
	out := CredentialAssurancesView{Credentials: []CredentialAssuranceView{}}
	if !s.canReadEnterprise(ctx, ep, actor) {
		return out, domain.ErrForbidden
	}
	st, err := s.assuranceStore()
	if errors.Is(err, domain.ErrFederationUnavailable) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	err = s.withIdentity(ctx, func(ctx context.Context) error {
		sess, err := s.assuranceBrowser(ctx, st, actor, ep, token)
		if err != nil {
			return err
		}
		policy, err := st.GetAssurancePolicy(ctx, ep)
		if err != nil {
			return err
		}
		out.Available, out.Policy = true, &policy
		proof, err := st.GetFederationSession(ctx, ep, sess.Token)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		now := time.Now().UTC()
		if err == nil && proof.UserID == actor && proof.SessionHash == sess.Token {
			valid, err := s.federationProofCurrent(ctx, st, proof, now)
			if err != nil {
				return err
			}
			deadline := proof.AuthenticatedAt.Add(domain.CredentialApprovalFreshness)
			if proof.ExpiresAt.Before(deadline) {
				deadline = proof.ExpiresAt
			}
			if valid && now.Before(deadline) {
				out.BrowserProof = &CredentialApprovalInput{proof.Protocol, proof.ConnectionRevision, proof.PolicyRevision}
				out.ApprovalAvailableUntil = &deadline
			}
		}
		out.Credentials, err = s.assuranceTargets(ctx, actor)
		if err != nil {
			return err
		}
		proofs, err := st.ListCredentialAssurances(ctx, ep, actor)
		if err != nil {
			return err
		}
		byID := map[string]domain.CredentialAssurance{}
		for _, p := range proofs {
			byID[p.CredentialID] = p
		}
		for i := range out.Credentials {
			v := &out.Credentials[i]
			a, ok := byID[v.ID]
			if !ok {
				continue
			}
			v.Protocol, v.AuthenticatedAt, v.VerifiedUntil = a.Proof.Protocol, &a.Proof.AuthenticatedAt, &a.Proof.ExpiresAt
			if a.RevokedAt != nil {
				v.State = "revoked"
				continue
			}
			if !now.Before(a.Proof.ExpiresAt) {
				v.State = "expired"
				continue
			}
			valid, err := s.federationProofCurrent(ctx, st, a.Proof, now)
			if err != nil {
				return err
			}
			v.State = "verification_changed"
			if valid {
				v.State = "approved"
			}
		}
		return nil
	})
	return out, err
}

func (s *IdentityService) ConfigureAssurancePolicy(ctx context.Context, actor, ep, token string, in AssurancePolicyInput) error {
	st, err := s.assuranceStore()
	if err != nil {
		return err
	}
	return s.withIdentity(ctx, func(ctx context.Context) error {
		if _, err := s.assuranceBrowser(ctx, st, actor, ep, token); err != nil {
			return err
		}
		if role, ok := s.EnterpriseRoleOf(ctx, ep, actor); !ok || role != domain.EnterpriseOwner {
			return domain.ErrForbidden
		}
		old, err := st.GetAssurancePolicy(ctx, ep)
		if err != nil {
			return err
		}
		if old.Revision != in.Revision {
			return domain.ErrConflict
		}
		p := domain.AssurancePolicy{EnterpriseID: ep, Revision: domain.NewID("ap_"), MaxAgeHours: in.MaxAgeHours}
		if err := p.Validate(); err != nil {
			return err
		}
		if old.MaxAgeHours == p.MaxAgeHours {
			return nil
		}
		if err := st.PutAssurancePolicy(ctx, p); err != nil {
			return err
		}
		return s.enterpriseAudit(ctx, ep, actor, "enterprise.assurance.policy_changed", p.Revision)
	})
}

func (s *IdentityService) ApproveCredential(ctx context.Context, actor, ep, token, target string, in CredentialApprovalInput) error {
	if domain.ValidateExternalID(target) != nil {
		return domain.ErrValidation
	}
	st, err := s.assuranceStore()
	if err != nil {
		return err
	}
	return s.withIdentity(ctx, func(ctx context.Context) error {
		sess, err := s.assuranceBrowser(ctx, st, actor, ep, token)
		if err != nil {
			return err
		}
		p, err := st.GetFederationSession(ctx, ep, sess.Token)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrRecentIdentityLogin
		}
		if err != nil {
			return err
		}
		if p.UserID != actor || p.SessionHash != sess.Token {
			return domain.ErrUnauthorized
		}
		if p.Protocol != in.Protocol || p.ConnectionRevision != in.ConnectionRevision || p.PolicyRevision != in.PolicyRevision {
			return domain.ErrConflict
		}
		now := time.Now().UTC()
		valid, err := s.federationProofCurrent(ctx, st, p, now)
		if err != nil {
			return err
		}
		if !valid || !now.Before(p.AuthenticatedAt.Add(domain.CredentialApprovalFreshness)) {
			return domain.ErrRecentIdentityLogin
		}
		// Lock the exact CLI row as well: deletion by existing device endpoints
		// then orders before or after this approval, even outside identity TX.
		sessions, err := s.repositories.ListSessionsForUser(ctx, actor)
		if err != nil {
			return err
		}
		for _, c := range sessions {
			if sessionKind(c) == "cli" && c.CredentialID == target {
				if _, err = st.LockFederationSession(ctx, c.Token); err != nil {
					return err
				}
				break
			}
		}
		targets, err := s.assuranceTargets(ctx, actor)
		if err != nil {
			return err
		}
		for _, t := range targets {
			if t.ID != target {
				continue
			}
			if t.ExpiresAt.Before(p.ExpiresAt) {
				p.ExpiresAt = t.ExpiresAt
			}
			previous, err := st.ListCredentialAssurances(ctx, ep, actor)
			if err != nil {
				return err
			}
			for _, old := range previous {
				q := old.Proof
				// Repeating approval from the same authentication must not
				// exploit a later refresh to remove an earlier lifetime cap.
				if old.CredentialID == target && q.Protocol == p.Protocol && q.ConnectionRevision == p.ConnectionRevision && q.PolicyRevision == p.PolicyRevision && q.AuthenticatedAt.Equal(p.AuthenticatedAt) && q.ExpiresAt.Before(p.ExpiresAt) {
					p.ExpiresAt = q.ExpiresAt
				}
			}
			if !now.Before(p.ExpiresAt) {
				return domain.ErrRecentIdentityLogin
			}
			a := domain.CredentialAssurance{EnterpriseID: ep, CredentialID: target, UserID: actor, Proof: p, ApprovedAt: now}
			if err := st.PutCredentialAssurance(ctx, a); err != nil {
				return err
			}
			return s.enterpriseAudit(ctx, ep, actor, "enterprise.credential.approved", target)
		}
		return domain.ErrNotFound
	})
}

func (s *IdentityService) RevokeCredentialAssurance(ctx context.Context, actor, ep, token, target string) error {
	if domain.ValidateExternalID(target) != nil {
		return domain.ErrValidation
	}
	st, err := s.assuranceStore()
	if err != nil {
		return err
	}
	return s.withIdentity(ctx, func(ctx context.Context) error {
		if _, err := s.assuranceBrowser(ctx, st, actor, ep, token); err != nil {
			return err
		}
		proofs, err := st.ListCredentialAssurances(ctx, ep, actor)
		if err != nil {
			return err
		}
		for _, a := range proofs {
			if a.CredentialID != target {
				continue
			}
			if a.RevokedAt != nil {
				return nil
			}
			now := time.Now().UTC()
			a.RevokedAt = &now
			if err := st.PutCredentialAssurance(ctx, a); err != nil {
				return err
			}
			return s.enterpriseAudit(ctx, ep, actor, "enterprise.credential.revoked", target)
		}
		return domain.ErrNotFound
	})
}
