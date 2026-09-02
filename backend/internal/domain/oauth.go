package domain

import (
	"fmt"
	"time"
)

// OAuthClient is a public OAuth 2.1 client registered through DCR. CXTHub's
// MCP clients use PKCE and token_endpoint_auth_method=none, so no client
// secret is stored.
type OAuthClient struct {
	ID           string    `json:"client_id"`
	Name         string    `json:"client_name"`
	RedirectURIs []string  `json:"redirect_uris"`
	CreatedAt    time.Time `json:"created_at"`
}

// OAuthAuthorizationRequest is the short-lived consent request created after
// validating /oauth/authorize. It contains no user identity until approval.
type OAuthAuthorizationRequest struct {
	ID            string    `json:"id"`
	ClientID      string    `json:"client_id"`
	RedirectURI   string    `json:"redirect_uri"`
	State         string    `json:"state"`
	CodeChallenge string    `json:"code_challenge"`
	Resource      string    `json:"resource"`
	Scope         string    `json:"scope"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// OAuthAuthorizationCode is stored only as a hash and consumed exactly once.
// Client, redirect, resource, and PKCE challenge remain bound to the original
// authorization request.
type OAuthAuthorizationCode struct {
	CodeHash      string    `json:"code_hash"`
	ClientID      string    `json:"client_id"`
	RedirectURI   string    `json:"redirect_uri"`
	UserID        string    `json:"user_id"`
	CodeChallenge string    `json:"code_challenge"`
	Resource      string    `json:"resource"`
	Scope         string    `json:"scope"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// OAuthTokenPair is returned only at issuance. Stores retain token hashes in
// the existing session table/files, never the bearer values.
type OAuthTokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	Scope        string
}

func ValidateOAuthClientRecord(client OAuthClient) error {
	if err := ValidateExternalID(client.ID); err != nil {
		return err
	}
	if client.Name == "" || len(client.Name) > 128 || len(client.RedirectURIs) == 0 || len(client.RedirectURIs) > 16 {
		return fmt.Errorf("%w: invalid OAuth client", ErrValidation)
	}
	for _, uri := range client.RedirectURIs {
		if uri == "" || len(uri) > 2048 {
			return fmt.Errorf("%w: invalid OAuth redirect URI", ErrValidation)
		}
	}
	return nil
}

func ValidateOAuthAuthorizationRequestRecord(req OAuthAuthorizationRequest) error {
	for _, value := range []string{req.ID, req.ClientID, req.RedirectURI, req.CodeChallenge, req.Resource, req.Scope} {
		if err := ValidateExternalID(value); err != nil {
			return err
		}
	}
	if req.ExpiresAt.IsZero() || !req.CreatedAt.Before(req.ExpiresAt) {
		return fmt.Errorf("%w: invalid OAuth request lifetime", ErrValidation)
	}
	return nil
}

func ValidateOAuthAuthorizationCodeRecord(code OAuthAuthorizationCode) error {
	for _, value := range []string{code.CodeHash, code.ClientID, code.RedirectURI, code.UserID, code.CodeChallenge, code.Resource, code.Scope} {
		if err := ValidateExternalID(value); err != nil {
			return err
		}
	}
	if code.ExpiresAt.IsZero() || !code.CreatedAt.Before(code.ExpiresAt) {
		return fmt.Errorf("%w: invalid OAuth code lifetime", ErrValidation)
	}
	return nil
}
