package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"strings"
	"time"
)

type samlConfig struct {
	provider outbound.SAMLProvider
	cipher   outbound.IdentityCipher
	origin   string
}

func (s *IdentityService) WithSAML(p outbound.SAMLProvider, c outbound.IdentityCipher, origin string) *IdentityService {
	s.saml = &samlConfig{p, c, strings.TrimRight(origin, "/")}
	return s
}

type SAMLConnectionInput struct {
	Domain   string `json:"domain"`
	Metadata string `json:"metadata"`
	Revision string `json:"revision"`
}
type SAMLView struct {
	Available            bool                        `json:"available"`
	Configured           bool                        `json:"configured"`
	Connection           *domain.SAMLConnection      `json:"connection,omitempty"`
	EntityID             string                      `json:"entity_id"`
	ACS                  string                      `json:"acs"`
	Linked               bool                        `json:"linked"`
	VerifiedUntil        *time.Time                  `json:"verified_until,omitempty"`
	SigningCertificate   *domain.SAMLCertificateInfo `json:"signing_certificate,omitempty"`
	AlternateCertificate *domain.SAMLCertificateInfo `json:"alternate_certificate,omitempty"`
}

func (s *IdentityService) samlStore() (outbound.SAMLStore, error) {
	st, ok := s.repositories.(outbound.SAMLStore)
	if !ok || s.saml == nil || s.saml.provider == nil || s.saml.cipher == nil {
		return nil, domain.ErrFederationUnavailable
	}
	return st, nil
}
func (s *IdentityService) samlEndpoints(id string) (string, string) {
	base := s.saml.origin + "/api/v1/auth/enterprise/saml/" + id
	return base + "/metadata", base + "/acs"
}
func (s *IdentityService) samlSettings(c domain.SAMLConnection) (outbound.SAMLSettings, error) {
	key, err := s.saml.cipher.Open("saml:"+c.EnterpriseID+":"+c.Revision, c.PrivateKey)
	if err != nil {
		return outbound.SAMLSettings{}, domain.ErrFederationUnavailable
	}
	entity, acs := s.samlEndpoints(c.EnterpriseID)
	settings := outbound.SAMLSettings{Metadata: c.Metadata, EntityID: entity, ACS: acs, Certificate: c.Certificate, PrivateKey: key}
	if c.Rotation != nil {
		settings.AdditionalCertificates = []string{c.Rotation.Certificate}
	}
	return settings, nil
}
func (s *IdentityService) verifiedSAMLDomain(ctx context.Context, c domain.SAMLConnection) error {
	return s.verifiedConnectionDomain(ctx, domain.OIDCConnection{EnterpriseID: c.EnterpriseID, Domain: c.Domain})
}
func (s *IdentityService) GetSAMLView(ctx context.Context, actor, id, token string) (SAMLView, error) {
	var out SAMLView
	if !s.canReadEnterprise(ctx, id, actor) {
		return out, domain.ErrForbidden
	}
	st, err := s.samlStore()
	if errors.Is(err, domain.ErrFederationUnavailable) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Available = true
	out.EntityID, out.ACS = s.samlEndpoints(id)
	c, err := st.GetSAMLConnection(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Configured = true
	info, err := domain.InspectSAMLCertificate(c.Certificate, time.Now())
	if err != nil {
		return out, err
	}
	out.SigningCertificate = &info
	if c.Rotation != nil {
		alternate, err := domain.InspectSAMLCertificate(c.Rotation.Certificate, time.Now())
		if err != nil {
			return out, err
		}
		out.AlternateCertificate = &alternate
		c.Rotation.PrivateKey = ""
	}
	c.Metadata = ""
	c.PrivateKey = ""
	out.Connection = &c
	_, err = st.GetFederationIdentity(ctx, id, "saml", actor)
	out.Linked = err == nil
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return out, err
	}
	if token != "" {
		sess, _, err := s.resolveSessionRecord(ctx, token)
		if err == nil && sess.Kind == "web" && sess.UserID == actor {
			proof, err := st.GetFederationSession(ctx, id, sess.Token)
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return out, err
			}
			if err == nil && proof.Protocol == "saml" && proof.UserID == actor && proof.ConnectionRevision == c.Revision && time.Now().Before(proof.ExpiresAt) && s.verifiedSAMLDomain(ctx, c) == nil {
				out.VerifiedUntil = &proof.ExpiresAt
			}
		}
	}
	return out, nil
}
func (s *IdentityService) ConfigureSAML(ctx context.Context, actor, id string, in SAMLConnectionInput) (SAMLView, error) {
	if in.Metadata == "" || len(in.Metadata) > 256<<10 {
		return SAMLView{}, domain.ErrValidation
	}
	st, err := s.samlStore()
	if err != nil {
		return SAMLView{}, err
	}
	if role, ok := s.EnterpriseRoleOf(ctx, id, actor); !ok || role != domain.EnterpriseOwner {
		return SAMLView{}, domain.ErrForbidden
	}
	name, err := domain.NormalizeEnterpriseDomain(in.Domain)
	if err != nil {
		return SAMLView{}, err
	}
	c := domain.SAMLConnection{EnterpriseID: id, Domain: name, Revision: domain.NewID("sc_"), Metadata: in.Metadata}
	if err = s.verifiedSAMLDomain(ctx, c); err != nil {
		return SAMLView{}, err
	}
	old, err := st.GetSAMLConnection(ctx, id)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return SAMLView{}, err
	}
	if old.Revision != in.Revision {
		return SAMLView{}, domain.ErrConflict
	}
	var key string
	if old.Revision != "" {
		settings, err := s.samlSettings(old)
		if err != nil {
			return SAMLView{}, err
		}
		c.Certificate, key = old.Certificate, settings.PrivateKey
	} else {
		c.Certificate, key, err = s.saml.provider.Keys(ctx)
		if err != nil {
			return SAMLView{}, domain.ErrFederationUnavailable
		}
	}
	entity, acs := s.samlEndpoints(id)
	c.Issuer, err = s.saml.provider.ValidateMetadata(ctx, outbound.SAMLSettings{Metadata: c.Metadata, EntityID: entity, ACS: acs, Certificate: c.Certificate, PrivateKey: key})
	if err != nil {
		return SAMLView{}, domain.ErrValidation
	}
	c.PrivateKey, err = s.saml.cipher.Seal("saml:"+id+":"+c.Revision, key)
	if err != nil {
		return SAMLView{}, domain.ErrFederationUnavailable
	}
	err = s.withIdentity(ctx, func(ctx context.Context) error {
		if role, ok := s.EnterpriseRoleOf(ctx, id, actor); !ok || role != domain.EnterpriseOwner {
			return domain.ErrForbidden
		}
		if err := s.verifiedSAMLDomain(ctx, c); err != nil {
			return err
		}
		cur, err := st.GetSAMLConnection(ctx, id)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if cur.Revision != in.Revision {
			return domain.ErrConflict
		}
		// IdP trust can change during rollover. Preserve both SP keys, including
		// any concurrent encryption rewrap, and require a new verification at
		// the updated revision before retiring the previous signer.
		if cur.Rotation != nil {
			r := *cur.Rotation
			r.VerifiedAt = nil
			c.Rotation = &r
		}
		if err = st.PutSAMLConnection(ctx, c); err != nil {
			return err
		}
		return s.enterpriseAudit(ctx, id, actor, "enterprise.saml.configured", id)
	})
	if err != nil {
		return SAMLView{}, err
	}
	return s.GetSAMLView(ctx, actor, id, "")
}
func (s *IdentityService) DisableSAML(ctx context.Context, actor, id, revision string) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		st, err := s.samlStore()
		if err != nil {
			return err
		}
		if role, ok := s.EnterpriseRoleOf(ctx, id, actor); !ok || role != domain.EnterpriseOwner {
			return domain.ErrForbidden
		}
		c, err := st.GetSAMLConnection(ctx, id)
		if err != nil {
			return err
		}
		if c.Revision != revision {
			return domain.ErrConflict
		}
		if err = st.DeleteSAMLConnection(ctx, id); err != nil {
			return err
		}
		return s.enterpriseAudit(ctx, id, actor, "enterprise.saml.disabled", id)
	})
}
func (s *IdentityService) SAMLMetadata(ctx context.Context, id string) (string, error) {
	st, err := s.samlStore()
	if err != nil {
		return "", err
	}
	c, err := st.GetSAMLConnection(ctx, id)
	if err != nil {
		return "", err
	}
	if err = s.verifiedSAMLDomain(ctx, c); err != nil {
		return "", err
	}
	settings, err := s.samlSettings(c)
	if err != nil {
		return "", err
	}
	return s.saml.provider.Metadata(ctx, settings)
}
func (s *IdentityService) checkSAMLAttempt(ctx context.Context, st outbound.SAMLStore, a domain.SAMLAttempt) (domain.SAMLConnection, domain.Session, error) {
	var zero domain.SAMLConnection
	sess, err := st.LockFederationSession(ctx, a.SessionHash)
	if err != nil || sess.UserID != a.UserID || sess.Kind != "web" || !time.Now().Before(sess.ExpiresAt) {
		return zero, sess, domain.ErrUnauthorized
	}
	if !s.canReadEnterprise(ctx, a.EnterpriseID, a.UserID) {
		return zero, sess, domain.ErrForbidden
	}
	c, err := st.GetSAMLConnection(ctx, a.EnterpriseID)
	if err != nil {
		return zero, sess, err
	}
	if c.Revision != a.ConnectionRevision || !time.Now().Before(a.ExpiresAt) {
		return zero, sess, domain.ErrConflict
	}
	if err = s.verifiedSAMLDomain(ctx, c); err != nil {
		return zero, sess, err
	}
	identity, err := st.GetFederationIdentity(ctx, a.EnterpriseID, "saml", a.UserID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return zero, sess, err
	}
	if errors.Is(err, domain.ErrNotFound) && time.Since(sess.CreatedAt) > 10*time.Minute {
		return zero, sess, domain.ErrRecentIdentityLogin
	}
	if err == nil && identity.Issuer != c.Issuer {
		return zero, sess, domain.ErrConflict
	}
	return c, sess, nil
}
func (s *IdentityService) BeginSAML(ctx context.Context, actor, id, token string) (OIDCAuthorization, error) {
	var out OIDCAuthorization
	st, err := s.samlStore()
	if err != nil {
		return out, err
	}
	if !strings.HasPrefix(token, "sess_") {
		return out, domain.ErrUnauthorized
	}
	sess, u, err := s.resolveSessionRecord(ctx, token)
	if err != nil || sess.Kind != "web" || u.ID != actor {
		return out, domain.ErrUnauthorized
	}
	state := newFederationRandom()
	now := time.Now().UTC()
	a := domain.SAMLAttempt{Hash: domain.HashToken(state), EnterpriseID: id, ConnectionRevision: "", UserID: actor, SessionHash: sess.Token, RequestID: "_" + newFederationRandom(), CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	var c domain.SAMLConnection
	err = s.withIdentity(ctx, func(ctx context.Context) error {
		c, err = st.GetSAMLConnection(ctx, id)
		if err != nil {
			return err
		}
		a.ConnectionRevision = c.Revision
		if _, _, err = s.checkSAMLAttempt(ctx, st, a); err != nil {
			return err
		}
		return st.CreateSAMLAttempt(ctx, a)
	})
	if err != nil {
		return out, err
	}
	settings, err := s.samlSettings(c)
	if err != nil {
		return out, err
	}
	out.URL, err = s.saml.provider.Authorize(ctx, settings, a.RequestID, state)
	if err != nil {
		return OIDCAuthorization{}, domain.ErrFederationUnavailable
	}
	return out, nil
}

// ReceiveSAML accepts no ambient authentication. It records a protocol proof,
// never an account link. Completion separately requires the initiating cookie.
func (s *IdentityService) ReceiveSAML(ctx context.Context, id, state string, raw []byte) (string, error) {
	st, err := s.samlStore()
	if err != nil {
		return "", err
	}
	if len(state) != 64 || len(raw) == 0 || len(raw) > 256<<10 {
		return "", domain.ErrValidation
	}
	var a domain.SAMLAttempt
	var c domain.SAMLConnection
	err = s.withIdentity(ctx, func(ctx context.Context) error {
		a, err = st.GetSAMLAttempt(ctx, domain.HashToken(state))
		if err != nil || a.EnterpriseID != id || a.Received || a.Completed {
			return domain.ErrUnauthorized
		}
		c, _, err = s.checkSAMLAttempt(ctx, st, a)
		return err
	})
	if err != nil {
		return "", err
	}
	settings, err := s.samlSettings(c)
	if err != nil {
		return "", err
	}
	proof, err := s.saml.provider.Verify(ctx, settings, a.RequestID, raw, a.CreatedAt)
	if err != nil {
		return "", domain.ErrUnauthorized
	}
	finish := newFederationRandom()
	err = s.withIdentity(ctx, func(ctx context.Context) error {
		a, err = st.GetSAMLAttempt(ctx, a.Hash)
		if err != nil {
			return err
		}
		if a.Received || a.Completed {
			return domain.ErrConflict
		}
		current, sess, err := s.checkSAMLAttempt(ctx, st, a)
		if err != nil {
			return err
		}
		if proof.Issuer != current.Issuer || proof.Subject == "" || len(proof.Subject) > 1024 || proof.AssertionID == "" || len(proof.AssertionID) > 256 || !time.Now().Before(proof.ExpiresAt) {
			return domain.ErrUnauthorized
		}
		expires := proof.ExpiresAt
		if sess.ExpiresAt.Before(expires) {
			expires = sess.ExpiresAt
		}
		a.Proof = domain.FederationSession{EnterpriseID: id, Protocol: "saml", SessionHash: a.SessionHash, ConnectionRevision: current.Revision, UserID: a.UserID, Issuer: proof.Issuer, Subject: proof.Subject, AuthenticatedAt: proof.AuthenticatedAt, ExpiresAt: expires, ACR: proof.ACR}
		a.AssertionID = proof.AssertionID
		a.Received = true
		a.FinishHash = domain.HashToken(finish)
		deadline := time.Now().Add(2 * time.Minute)
		if deadline.Before(a.ExpiresAt) {
			a.ExpiresAt = deadline
		}
		if expires.Before(a.ExpiresAt) {
			a.ExpiresAt = expires
		}
		return st.ReceiveSAMLAttempt(ctx, a)
	})
	if err != nil {
		return "", err
	}
	return finish, nil
}
func (s *IdentityService) CompleteSAML(ctx context.Context, token, finish string) (string, error) {
	st, err := s.samlStore()
	if err != nil {
		return "", err
	}
	if len(finish) != 64 || !strings.HasPrefix(token, "sess_") {
		return "", domain.ErrUnauthorized
	}
	return identityResult(ctx, s, func(ctx context.Context) (string, error) {
		a, err := st.GetSAMLFinish(ctx, domain.HashToken(finish))
		if err != nil || !a.Received || a.Completed || a.SessionHash != domain.HashToken(token) {
			return "", domain.ErrUnauthorized
		}
		current, _, err := s.checkSAMLAttempt(ctx, st, a)
		if err != nil {
			return "", err
		}
		if a.Proof.Issuer != current.Issuer || a.Proof.UserID != a.UserID || !time.Now().Before(a.Proof.ExpiresAt) {
			return "", domain.ErrUnauthorized
		}
		identity := domain.FederationIdentity{EnterpriseID: a.EnterpriseID, Protocol: "saml", Issuer: a.Proof.Issuer, Subject: a.Proof.Subject, UserID: a.UserID, CreatedAt: time.Now().UTC()}
		if err = st.PutFederationIdentity(ctx, identity); err != nil {
			return "", err
		}
		if err = st.PutFederationSession(ctx, a.Proof); err != nil {
			return "", err
		}
		if err = st.FinishSAMLAttempt(ctx, a.Hash); err != nil {
			return "", err
		}
		// Current-revision completion proves a post-activation roundtrip. It
		// does not prove the IdP enforced request signatures; the owner still
		// controls retirement. This write shares binding/consumption/audit.
		if current.Rotation != nil && current.Rotation.State == "active" && current.Rotation.VerifiedAt == nil {
			now := time.Now().UTC()
			current.Rotation.VerifiedAt = &now
			if err = st.PutSAMLConnection(ctx, current); err != nil {
				return "", err
			}
		}
		if err = s.enterpriseAudit(ctx, a.EnterpriseID, a.UserID, "enterprise.saml.authenticated", a.EnterpriseID); err != nil {
			return "", err
		}
		es, _ := s.enterpriseStore()
		ep, err := es.GetEnterprise(ctx, a.EnterpriseID)
		return ep.Slug, err
	})
}
