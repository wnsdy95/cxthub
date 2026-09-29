package domain

import "time"

// Federation connects an existing, explicitly authenticated CXTHub account to
// an external subject. None of these records grants organization membership.
type OIDCConnection struct {
	EnterpriseID string `json:"enterprise_id"`
	Domain       string `json:"domain"`
	Issuer       string `json:"issuer"`
	ClientID     string `json:"client_id"`
	AuthMethod   string `json:"auth_method"`
	Revision     string `json:"revision"`
	Secret       string `json:"-"`
}
type OIDCAttempt struct {
	Hash, EnterpriseID, ConnectionRevision, UserID, SessionHash, Nonce, Verifier string
	CreatedAt, ExpiresAt                                                         time.Time
	Consumed                                                                     bool
}
type FederationIdentity struct {
	EnterpriseID, Protocol, Issuer, Subject, UserID string
	CreatedAt                                       time.Time
}
type FederationSession struct {
	EnterpriseID, Protocol, SessionHash, ConnectionRevision, UserID, Issuer, Subject, ACR string
	AMR                                                                                   []string
	AuthenticatedAt, ExpiresAt                                                            time.Time
	// Protocol expiry governs initial acceptance, not authenticated-session life.
	ProofExpiresAt                 time.Time
	IdPSessionExpiresAt            *time.Time
	PolicyRevision, DomainRevision string
}
