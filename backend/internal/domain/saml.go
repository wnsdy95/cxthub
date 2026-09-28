package domain

import "time"

type SAMLConnection struct {
	EnterpriseID string `json:"enterprise_id"`
	Domain       string `json:"domain"`
	Issuer       string `json:"issuer"`
	Revision     string `json:"revision"`
	Certificate  string `json:"certificate"`
	Metadata     string `json:"-"`
	PrivateKey   string `json:"-"`
}

// The raw response is never persisted. A validated proof awaits a separate
// browser-bound completion, since cross-site POSTs cannot rely on a Lax cookie.
type SAMLAttempt struct {
	Hash, EnterpriseID, ConnectionRevision, UserID, SessionHash, RequestID string
	CreatedAt, ExpiresAt                                                   time.Time
	Received, Completed                                                    bool
	FinishHash, AssertionID                                                string
	Proof                                                                  FederationSession
}
