package domain

import "time"

// OAuthGrant identifies one authorization, not every connection by a user or
// client. CreatedAt is authorization issuance, never evidence of IdP/MFA login.
type OAuthGrant struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	ClientID  string     `json:"client_id"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

func (g OAuthGrant) Active(now time.Time) bool {
	return g.RevokedAt == nil && now.Before(g.ExpiresAt)
}

func ValidateOAuthGrant(g OAuthGrant) error {
	if ValidateExternalID(g.ID) != nil || ValidateExternalID(g.UserID) != nil || ValidateExternalID(g.ClientID) != nil || g.CreatedAt.IsZero() || !g.ExpiresAt.After(g.CreatedAt) {
		return ErrValidation
	}
	if g.RevokedAt != nil && g.RevokedAt.Before(g.CreatedAt) {
		return ErrValidation
	}
	return nil
}

// OAuthRefreshReference survives rotation of the active session. It contains
// only a token hash, and permits replay detection until that token's expiry.
type OAuthRefreshReference struct {
	TokenHash string    `json:"token_hash"`
	GrantID   string    `json:"grant_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func ValidateOAuthRefreshReference(r OAuthRefreshReference) error {
	if len(r.TokenHash) != 68 || r.TokenHash[:4] != "tkh_" || ValidateStoredSessionToken(r.TokenHash) != nil || ValidateExternalID(r.GrantID) != nil || r.ExpiresAt.IsZero() {
		return ErrValidation
	}
	return nil
}
