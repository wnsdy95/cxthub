package app

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type MCPApplication struct {
	ClientID  string    `json:"client_id"`
	Name      string    `json:"name"`
	Scope     string    `json:"scope"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Consumption, token creation, and revocation use the same identity transaction.
// Revoking an application also invalidates unexchanged authorization codes.
func (s *IdentityService) ExchangeMCPAuthorizationCode(ctx context.Context, hash, client, redirect, challenge, resource string) (domain.OAuthTokenPair, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.OAuthTokenPair, error) {
		oauth, ok := s.repositories.(outbound.OAuthStore)
		if !ok {
			return domain.OAuthTokenPair{}, domain.ErrUnauthorized
		}
		audit, ok := s.repositories.(outbound.OAuthManagement)
		if !ok {
			return domain.OAuthTokenPair{}, domain.ErrUnauthorized
		}
		code, err := oauth.ConsumeOAuthAuthorizationCode(ctx, hash, client, redirect, challenge)
		if err != nil || code.Resource != resource || code.Scope != "mcp:read" {
			return domain.OAuthTokenPair{}, domain.ErrUnauthorized
		}
		pair, err := s.issueMCPTokenPair(ctx, code.UserID, client)
		if err != nil {
			return domain.OAuthTokenPair{}, err
		}
		err = audit.AppendAccountAudit(ctx, domain.AccountAuditEvent{ID: domain.NewID("aud_"), UserID: code.UserID, ClientID: client, Action: "mcp.authorized", CreatedAt: time.Now().UTC()})
		return pair, err
	})
}
func (s *IdentityService) ListMCPApplications(ctx context.Context, user string) ([]MCPApplication, error) {
	sessions, err := s.repositories.ListSessionsForUser(ctx, user)
	if err != nil {
		return nil, err
	}
	oauth, ok := s.repositories.(outbound.OAuthStore)
	if !ok {
		return nil, domain.ErrForbidden
	}
	byClient := map[string]MCPApplication{}
	for _, session := range sessions {
		if session.Kind != "mcp_access" && session.Kind != "mcp_refresh" || !time.Now().Before(session.ExpiresAt) {
			continue
		}
		item, ok := byClient[session.Label]
		if !ok {
			client, err := oauth.GetOAuthClient(ctx, session.Label)
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return nil, err
			}
			name := client.Name
			if name == "" {
				name = session.Label
			}
			item = MCPApplication{ClientID: session.Label, Name: name, Scope: "mcp:read", CreatedAt: session.CreatedAt, ExpiresAt: session.ExpiresAt}
		}
		if session.CreatedAt.Before(item.CreatedAt) {
			item.CreatedAt = session.CreatedAt
		}
		if session.ExpiresAt.After(item.ExpiresAt) {
			item.ExpiresAt = session.ExpiresAt
		}
		byClient[session.Label] = item
	}
	out := []MCPApplication{}
	for _, item := range byClient {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClientID < out[j].ClientID })
	return out, nil
}
func (s *IdentityService) RevokeMCPApplication(ctx context.Context, user, client string) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		if domain.ValidateExternalID(client) != nil {
			return domain.ErrValidation
		}
		management, ok := s.repositories.(outbound.OAuthManagement)
		if !ok {
			return domain.ErrForbidden
		}
		sessions, err := s.repositories.ListSessionsForUser(ctx, user)
		if err != nil {
			return err
		}
		for _, session := range sessions {
			if session.Label == client && (session.Kind == "mcp_access" || session.Kind == "mcp_refresh") {
				if err = s.repositories.DeleteSession(ctx, session.Token); err != nil {
					return err
				}
			}
		}
		if err = management.DeleteOAuthClientCodes(ctx, user, client); err != nil {
			return err
		}
		return management.AppendAccountAudit(ctx, domain.AccountAuditEvent{ID: domain.NewID("aud_"), UserID: user, ClientID: client, Action: "mcp.revoked", CreatedAt: time.Now().UTC()})
	})
}
func (s *IdentityService) AccountAudit(ctx context.Context, user string) ([]domain.AccountAuditEvent, error) {
	st, ok := s.repositories.(outbound.OAuthManagement)
	if !ok {
		return nil, domain.ErrForbidden
	}
	return st.ListAccountAudit(ctx, user)
}
