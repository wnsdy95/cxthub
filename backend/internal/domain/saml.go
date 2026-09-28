package domain

import "time"

type SAMLConnection struct {
	EnterpriseID string               `json:"enterprise_id"`
	Domain       string               `json:"domain"`
	Issuer       string               `json:"issuer"`
	Revision     string               `json:"revision"`
	Certificate  string               `json:"certificate"`
	Metadata     string               `json:"-"`
	PrivateKey   string               `json:"-"`
	Rotation     *SAMLSigningRotation `json:"rotation,omitempty"`
}

// The alternate is the upcoming key while prepared, then the previous key
// while active. Its encryption purpose uses ID, independently of config edits.
type SAMLSigningRotation struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	Certificate string     `json:"certificate"`
	PrivateKey  string     `json:"-"`
	CreatedAt   time.Time  `json:"created_at"`
	ActivatedAt *time.Time `json:"activated_at,omitempty"`
	VerifiedAt  *time.Time `json:"verified_at,omitempty"`
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
