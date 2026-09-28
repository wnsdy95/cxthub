package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"time"
)

type SAMLSigningInput struct {
	Revision       string `json:"revision"`
	Action         string `json:"action"`
	TrustConfirmed bool   `json:"trust_confirmed"`
}

// ChangeSAMLSigning owns the rollover state machine. The browser submits an
// intention and editor revision, never a private key or a computed authority.
func (s *IdentityService) ChangeSAMLSigning(ctx context.Context, actor, id string, in SAMLSigningInput) (SAMLView, error) {
	switch in.Action {
	case "prepare", "activate", "cancel", "rollback", "retire":
	default:
		return SAMLView{}, domain.ErrValidation
	}
	if in.Revision == "" {
		return SAMLView{}, domain.ErrValidation
	}
	st, err := s.samlStore()
	if err != nil {
		return SAMLView{}, err
	}
	owner := func(ctx context.Context) bool {
		r, ok := s.EnterpriseRoleOf(ctx, id, actor)
		return ok && r == domain.EnterpriseOwner
	}
	if !owner(ctx) {
		return SAMLView{}, domain.ErrForbidden
	}
	observed, err := st.GetSAMLConnection(ctx, id)
	if err != nil {
		return SAMLView{}, err
	}
	if observed.Revision != in.Revision {
		return SAMLView{}, domain.ErrConflict
	}
	var nextCert, nextKey string
	if in.Action == "prepare" {
		if observed.Rotation != nil {
			return SAMLView{}, domain.ErrConflict
		}
		// Generate outside the global identity lock. Recheck authority/state below.
		nextCert, nextKey, err = s.saml.provider.Keys(ctx)
		if err != nil {
			return SAMLView{}, domain.ErrFederationUnavailable
		}
	}
	err = s.withIdentity(ctx, func(ctx context.Context) error {
		if !owner(ctx) {
			return domain.ErrForbidden
		}
		c, err := st.GetSAMLConnection(ctx, id)
		if err != nil {
			return err
		}
		if c.Revision != in.Revision {
			return domain.ErrConflict
		}
		settings, err := s.samlSettings(c)
		if err != nil {
			return err
		}
		activeKey := settings.PrivateKey
		purpose := func(rotation string) string { return "saml-rotation:" + id + ":" + rotation }
		validate := func(cert, key string) error {
			candidate := settings
			candidate.Certificate = cert
			candidate.PrivateKey = key
			candidate.AdditionalCertificates = nil
			issuer, err := s.saml.provider.Validate(ctx, candidate)
			if err != nil || issuer != c.Issuer {
				return domain.ErrValidation
			}
			return nil
		}
		now := time.Now().UTC()
		auditTarget := id
		switch in.Action {
		case "prepare":
			if c.Rotation != nil {
				return domain.ErrConflict
			}
			if err = s.verifiedSAMLDomain(ctx, c); err != nil {
				return err
			}
			if err = validate(nextCert, nextKey); err != nil {
				return err
			}
			r := &domain.SAMLSigningRotation{ID: domain.NewID("sr_"), State: "prepared", Certificate: nextCert, CreatedAt: now}
			r.PrivateKey, err = s.saml.cipher.Seal(purpose(r.ID), nextKey)
			if err != nil {
				return domain.ErrFederationUnavailable
			}
			c.Rotation = r
			auditTarget = r.ID
		case "activate", "rollback":
			r := c.Rotation
			if r == nil || (in.Action == "activate" && r.State != "prepared") || (in.Action == "rollback" && r.State != "active") {
				return domain.ErrConflict
			}
			if in.Action == "activate" && !in.TrustConfirmed {
				return domain.ErrValidation
			}
			if err = s.verifiedSAMLDomain(ctx, c); err != nil {
				return err
			}
			alternate, err := s.saml.cipher.Open(purpose(r.ID), r.PrivateKey)
			if err != nil {
				return domain.ErrFederationUnavailable
			}
			if err = validate(r.Certificate, alternate); err != nil {
				return err
			}
			auditTarget = r.ID
			c.Certificate, r.Certificate = r.Certificate, c.Certificate
			if in.Action == "activate" {
				r.PrivateKey, err = s.saml.cipher.Seal(purpose(r.ID), activeKey)
				if err != nil {
					return domain.ErrFederationUnavailable
				}
				r.State = "active"
				r.ActivatedAt = &now
				r.VerifiedAt = nil
			} else {
				c.Rotation = nil
			}
			activeKey = alternate
		case "cancel", "retire":
			r := c.Rotation
			if r == nil || (in.Action == "cancel" && r.State != "prepared") || (in.Action == "retire" && (r.State != "active" || r.VerifiedAt == nil)) {
				return domain.ErrConflict
			}
			if in.Action == "retire" {
				if err = validate(c.Certificate, activeKey); err != nil {
					return err
				}
			}
			auditTarget = r.ID
			c.Rotation = nil
		}
		// Every lifecycle action invalidates outstanding attempts and proofs. A
		// completion can mark verification without changing this editor revision.
		c.Revision = domain.NewID("sc_")
		c.PrivateKey, err = s.saml.cipher.Seal("saml:"+id+":"+c.Revision, activeKey)
		if err != nil {
			return domain.ErrFederationUnavailable
		}
		if err = st.PutSAMLConnection(ctx, c); err != nil {
			return err
		}
		return s.enterpriseAudit(ctx, id, actor, "enterprise.saml.signing."+in.Action, auditTarget)
	})
	if err != nil {
		return SAMLView{}, err
	}
	return s.GetSAMLView(ctx, actor, id, "")
}
