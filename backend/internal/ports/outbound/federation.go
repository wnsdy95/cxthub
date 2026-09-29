package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// OIDCSettings is private transport configuration, never an HTTP response.
type OIDCSettings struct {
	Issuer, ClientID, RedirectURI, AuthMethod string
	ClientSecret                              string `json:"-"`
}

// FederationProof describes one verified authentication, not an access grant.
// Issuer and subject are identity keys. Email is deliberately not a linking key.
type FederationProof struct {
	Issuer, Subject, ACR       string
	AMR                        []string
	AuthenticatedAt, ExpiresAt time.Time
	// Optional SAML SessionNotOnOrAfter. Distinct from assertion/token expiry.
	SessionExpiresAt *time.Time
}

// OIDCProvider validates the protocol. The application must separately bind and
// consume state, check the connection revision, and recheck the target session.
type OIDCProvider interface {
	Authorize(context.Context, OIDCSettings, string, string, string) (string, error)
	Exchange(context.Context, OIDCSettings, string, string, string, time.Time) (FederationProof, error)
}

type IdentityCipher interface {
	Seal(string, string) (string, error)
	Open(string, string) (string, error)
}

// Mutations and session locks require the identity transaction. External
// network exchanges happen between transactions, never while holding a lock.
type FederationStore interface {
	IdentityTransactions
	GetOIDCConnection(context.Context, string) (domain.OIDCConnection, error)
	PutOIDCConnection(context.Context, domain.OIDCConnection) error
	DeleteOIDCConnection(context.Context, string) error
	CreateOIDCAttempt(context.Context, domain.OIDCAttempt) error
	GetOIDCAttempt(context.Context, string) (domain.OIDCAttempt, error)
	ConsumeOIDCAttempt(context.Context, string) error
	LockFederationSession(context.Context, string) (domain.Session, error)
	GetFederationIdentity(context.Context, string, string, string) (domain.FederationIdentity, error)
	PutFederationIdentity(context.Context, domain.FederationIdentity) error
	GetFederationSession(context.Context, string, string) (domain.FederationSession, error)
	PutFederationSession(context.Context, domain.FederationSession) error
}
