package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type federationConfig struct {
	provider outbound.OIDCProvider
	cipher   outbound.IdentityCipher
	callback string
}

func (s *IdentityService) WithOIDC(provider outbound.OIDCProvider, cipher outbound.IdentityCipher, origin string) *IdentityService {
	s.federation = &federationConfig{provider, cipher, strings.TrimRight(origin, "/") + "/api/v1/auth/enterprise/oidc/callback"}
	return s
}

type OIDCConnectionInput struct {
	Domain       string `json:"domain"`
	Issuer       string `json:"issuer"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	AuthMethod   string `json:"auth_method"`
	Revision     string `json:"revision"`
}
type OIDCView struct {
	Available     bool                   `json:"available"`
	Configured    bool                   `json:"configured"`
	Connection    *domain.OIDCConnection `json:"connection,omitempty"`
	CallbackURI   string                 `json:"callback_uri"`
	Linked        bool                   `json:"linked"`
	VerifiedUntil *time.Time             `json:"verified_until,omitempty"`
}
type OIDCAuthorization struct {
	URL string `json:"url"`
}

func (s *IdentityService) federationStore() (outbound.FederationStore, error) {
	st, ok := s.repositories.(outbound.FederationStore)
	if !ok || s.federation == nil || s.federation.provider == nil || s.federation.cipher == nil {
		return nil, domain.ErrFederationUnavailable
	}
	return st, nil
}
func (s *IdentityService) verifiedConnectionDomain(ctx context.Context, c domain.OIDCConnection) error {
	st, ok := s.repositories.(outbound.EnterpriseDomainStore)
	if !ok {
		return domain.ErrFederationUnavailable
	}
	d, err := st.GetEnterpriseDomain(ctx, c.EnterpriseID, c.Domain)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrVerifiedDomainRequired
	}
	if err != nil {
		return err
	}
	if !d.Verified(time.Now()) {
		return domain.ErrVerifiedDomainRequired
	}
	return nil
}
func (s *IdentityService) oidcSettings(c domain.OIDCConnection) (outbound.OIDCSettings, error) {
	secret, err := s.federation.cipher.Open(c.EnterpriseID+":"+c.Revision, c.Secret)
	if err != nil {
		return outbound.OIDCSettings{}, domain.ErrFederationUnavailable
	}
	return outbound.OIDCSettings{Issuer: c.Issuer, ClientID: c.ClientID, ClientSecret: secret, AuthMethod: c.AuthMethod, RedirectURI: s.federation.callback}, nil
}
func (s *IdentityService) GetOIDCView(ctx context.Context, actor, id, token string) (OIDCView, error) {
	var out OIDCView
	if !s.canReadEnterprise(ctx, id, actor) {
		return out, domain.ErrForbidden
	}
	st, err := s.federationStore()
	if errors.Is(err, domain.ErrFederationUnavailable) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Available = true
	out.CallbackURI = s.federation.callback
	c, err := st.GetOIDCConnection(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Configured = true
	c.Secret = ""
	out.Connection = &c
	_, err = st.GetFederationIdentity(ctx, id, "oidc", actor)
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
			if err == nil && proof.UserID == actor && proof.ConnectionRevision == c.Revision && time.Now().Before(proof.ExpiresAt) && s.verifiedConnectionDomain(ctx, c) == nil {
				out.VerifiedUntil = &proof.ExpiresAt
			}
		}
	}
	return out, nil
}
func (s *IdentityService) ConfigureOIDC(ctx context.Context, actor, id string, in OIDCConnectionInput) (OIDCView, error) {
	if len(in.Issuer) > 2048 || in.ClientID == "" || len(in.ClientID) > 256 || in.ClientSecret == "" || len(in.ClientSecret) > 4096 {
		return OIDCView{}, domain.ErrValidation
	}
	st, err := s.federationStore()
	if err != nil {
		return OIDCView{}, err
	}
	if role, ok := s.EnterpriseRoleOf(ctx, id, actor); !ok || role != domain.EnterpriseOwner {
		return OIDCView{}, domain.ErrForbidden
	}
	name, err := domain.NormalizeEnterpriseDomain(in.Domain)
	if err != nil {
		return OIDCView{}, err
	}
	if in.AuthMethod == "" {
		in.AuthMethod = "client_secret_basic"
	}
	c := domain.OIDCConnection{EnterpriseID: id, Domain: name, Issuer: strings.TrimSpace(in.Issuer), ClientID: strings.TrimSpace(in.ClientID), AuthMethod: in.AuthMethod, Revision: domain.NewID("oc_")}
	if err = s.verifiedConnectionDomain(ctx, c); err != nil {
		return OIDCView{}, err
	}
	settings := outbound.OIDCSettings{Issuer: c.Issuer, ClientID: c.ClientID, ClientSecret: in.ClientSecret, AuthMethod: c.AuthMethod, RedirectURI: s.federation.callback}
	if _, err = s.federation.provider.Authorize(ctx, settings, newFederationRandom(), newFederationRandom(), newFederationRandom()); err != nil {
		return OIDCView{}, domain.ErrValidation
	}
	c.Secret, err = s.federation.cipher.Seal(id+":"+c.Revision, in.ClientSecret)
	if err != nil {
		return OIDCView{}, domain.ErrFederationUnavailable
	}
	err = s.withIdentity(ctx, func(ctx context.Context) error {
		if role, ok := s.EnterpriseRoleOf(ctx, id, actor); !ok || role != domain.EnterpriseOwner {
			return domain.ErrForbidden
		}
		if err := s.verifiedConnectionDomain(ctx, c); err != nil {
			return err
		}
		old, err := st.GetOIDCConnection(ctx, id)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if old.Revision != in.Revision {
			return domain.ErrConflict
		}
		if err = st.PutOIDCConnection(ctx, c); err != nil {
			return err
		}
		return s.enterpriseAudit(ctx, id, actor, "enterprise.oidc.configured", id)
	})
	if err != nil {
		return OIDCView{}, err
	}
	return s.GetOIDCView(ctx, actor, id, "")
}
func (s *IdentityService) DisableOIDC(ctx context.Context, actor, id, revision string) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		st, err := s.federationStore()
		if err != nil {
			return err
		}
		if role, ok := s.EnterpriseRoleOf(ctx, id, actor); !ok || role != domain.EnterpriseOwner {
			return domain.ErrForbidden
		}
		c, err := st.GetOIDCConnection(ctx, id)
		if err != nil {
			return err
		}
		if c.Revision != revision {
			return domain.ErrConflict
		}
		if err = st.DeleteOIDCConnection(ctx, id); err != nil {
			return err
		}
		return s.enterpriseAudit(ctx, id, actor, "enterprise.oidc.disabled", id)
	})
}
func newFederationRandom() string { return domain.NewID("") + domain.NewID("") }

func (s *IdentityService) checkFederationAttempt(ctx context.Context, st outbound.FederationStore, a domain.OIDCAttempt) (domain.OIDCConnection, domain.Session, error) {
	var zero domain.OIDCConnection
	sess, err := st.LockFederationSession(ctx, a.SessionHash)
	if err != nil || sess.UserID != a.UserID || sess.Kind != "web" || !time.Now().Before(sess.ExpiresAt) {
		return zero, sess, domain.ErrUnauthorized
	}
	if !s.canReadEnterprise(ctx, a.EnterpriseID, a.UserID) {
		return zero, sess, domain.ErrForbidden
	}
	c, err := st.GetOIDCConnection(ctx, a.EnterpriseID)
	if err != nil {
		return zero, sess, err
	}
	if c.Revision != a.ConnectionRevision || !time.Now().Before(a.ExpiresAt) {
		return zero, sess, domain.ErrConflict
	}
	if err = s.verifiedConnectionDomain(ctx, c); err != nil {
		return zero, sess, err
	}
	identity, err := st.GetFederationIdentity(ctx, a.EnterpriseID, "oidc", a.UserID)
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
func (s *IdentityService) BeginOIDC(ctx context.Context, actor, id, token string) (OIDCAuthorization, error) {
	var out OIDCAuthorization
	st, err := s.federationStore()
	if err != nil {
		return out, err
	}
	if !strings.HasPrefix(token, "sess_") {
		return out, domain.ErrUnauthorized
	}
	sess, user, err := s.resolveSessionRecord(ctx, token)
	if err != nil || sess.Kind != "web" || user.ID != actor {
		return out, domain.ErrUnauthorized
	}
	if !s.canReadEnterprise(ctx, id, actor) {
		return out, domain.ErrForbidden
	}
	c, err := st.GetOIDCConnection(ctx, id)
	if err != nil {
		return out, err
	}
	if err = s.verifiedConnectionDomain(ctx, c); err != nil {
		return out, err
	}
	settings, err := s.oidcSettings(c)
	if err != nil {
		return out, err
	}
	state, nonce, verifier := newFederationRandom(), newFederationRandom(), newFederationRandom()
	now := time.Now().UTC()
	a := domain.OIDCAttempt{Hash: domain.HashToken(state), EnterpriseID: id, ConnectionRevision: c.Revision, UserID: actor, SessionHash: sess.Token, Nonce: nonce, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	a.Verifier, err = s.federation.cipher.Seal(a.Hash, verifier)
	if err != nil {
		return out, domain.ErrFederationUnavailable
	}
	out.URL, err = s.federation.provider.Authorize(ctx, settings, state, nonce, verifier)
	if err != nil {
		return OIDCAuthorization{}, domain.ErrFederationUnavailable
	}
	err = s.withIdentity(ctx, func(ctx context.Context) error {
		if _, _, err := s.checkFederationAttempt(ctx, st, a); err != nil {
			return err
		}
		return st.CreateOIDCAttempt(ctx, a)
	})
	if err != nil {
		return OIDCAuthorization{}, err
	}
	return out, nil
}
func (s *IdentityService) CompleteOIDC(ctx context.Context, token, state, code string) (string, error) {
	st, err := s.federationStore()
	if err != nil {
		return "", err
	}
	if len(state) != 64 || code == "" || len(code) > 4096 {
		return "", domain.ErrValidation
	}
	var a domain.OIDCAttempt
	var c domain.OIDCConnection
	err = s.withIdentity(ctx, func(ctx context.Context) error {
		a, err = st.GetOIDCAttempt(ctx, domain.HashToken(state))
		if err != nil {
			return domain.ErrUnauthorized
		}
		if a.Consumed || a.SessionHash != domain.HashToken(token) {
			return domain.ErrUnauthorized
		}
		c, _, err = s.checkFederationAttempt(ctx, st, a)
		if err != nil {
			return err
		}
		return st.ConsumeOIDCAttempt(ctx, a.Hash)
	})
	if err != nil {
		return "", err
	}
	settings, err := s.oidcSettings(c)
	if err != nil {
		return "", err
	}
	verifier, err := s.federation.cipher.Open(a.Hash, a.Verifier)
	if err != nil {
		return "", domain.ErrFederationUnavailable
	}
	proof, err := s.federation.provider.Exchange(ctx, settings, code, a.Nonce, verifier, a.CreatedAt)
	if err != nil {
		return "", domain.ErrUnauthorized
	}
	return identityResult(ctx, s, func(ctx context.Context) (string, error) {
		current, sess, err := s.checkFederationAttempt(ctx, st, a)
		if err != nil {
			return "", err
		}
		if proof.Issuer != current.Issuer || proof.Subject == "" || !time.Now().Before(proof.ExpiresAt) {
			return "", domain.ErrUnauthorized
		}
		identity := domain.FederationIdentity{EnterpriseID: a.EnterpriseID, Protocol: "oidc", Issuer: proof.Issuer, Subject: proof.Subject, UserID: a.UserID, CreatedAt: time.Now().UTC()}
		if err = st.PutFederationIdentity(ctx, identity); err != nil {
			return "", err
		}
		expires := proof.ExpiresAt
		if sess.ExpiresAt.Before(expires) {
			expires = sess.ExpiresAt
		}
		if err = st.PutFederationSession(ctx, domain.FederationSession{EnterpriseID: a.EnterpriseID, Protocol: "oidc", SessionHash: a.SessionHash, ConnectionRevision: current.Revision, UserID: a.UserID, Issuer: proof.Issuer, Subject: proof.Subject, AuthenticatedAt: proof.AuthenticatedAt, ExpiresAt: expires, AMR: proof.AMR, ACR: proof.ACR}); err != nil {
			return "", err
		}
		if err = s.enterpriseAudit(ctx, a.EnterpriseID, a.UserID, "enterprise.oidc.authenticated", a.EnterpriseID); err != nil {
			return "", err
		}
		enterprises, _ := s.enterpriseStore()
		e, err := enterprises.GetEnterprise(ctx, a.EnterpriseID)
		return e.Slug, err
	})
}
