package auth

import (
	"context"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// DevVerifier is a local/demo IdentityVerifier (works without Firebase).
//
// Token format: "dev:<email>" or "dev:<email>:<name>". Never use in production
// (only injected when cmd has CXT_AUTH=dev). Actual authentication is done by FirebaseVerifier.
type DevVerifier struct{}

// NewDevVerifier creates a DevVerifier.
func NewDevVerifier() *DevVerifier { return &DevVerifier{} }

var _ outbound.IdentityVerifier = (*DevVerifier)(nil)

// Verify parses "dev:<email>[:<name>]" tokens into Users. Format mismatch → ErrUnauthorized.
func (d *DevVerifier) Verify(_ context.Context, token string) (domain.User, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(token), "dev:")
	if !ok || rest == "" {
		return domain.User{}, domain.ErrUnauthorized
	}
	parts := strings.SplitN(rest, ":", 2)
	email := parts[0]
	name := email
	if len(parts) == 2 && parts[1] != "" {
		name = parts[1]
	}
	return domain.User{ID: "dev:" + email, Email: email, Name: name}, nil
}
