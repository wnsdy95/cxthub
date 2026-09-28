package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *IdentityService) grantStore() (outbound.OAuthGrants, error) {
	st, ok := s.repositories.(outbound.OAuthGrants)
	if !ok {
		return nil, domain.ErrForbidden
	}
	return st, nil
}

func (s *IdentityService) issueMCPTokenPair(ctx context.Context, user, client string) (domain.OAuthTokenPair, error) {
	st, err := s.grantStore()
	if err != nil {
		return domain.OAuthTokenPair{}, err
	}
	if _, err = s.repositories.GetUser(ctx, user); err != nil {
		return domain.OAuthTokenPair{}, domain.ErrUnauthorized
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	g := domain.OAuthGrant{ID: domain.NewID("grt_"), UserID: user, ClientID: client, CreatedAt: now, ExpiresAt: now.Add(mcpRefreshTokenTTL)}
	if err = st.CreateOAuthGrant(ctx, g); err != nil {
		return domain.OAuthTokenPair{}, err
	}
	return s.issueGrantPair(ctx, st, g)
}

func (s *IdentityService) issueGrantPair(ctx context.Context, st outbound.OAuthGrants, g domain.OAuthGrant) (out domain.OAuthTokenPair, err error) {
	access, err := s.issueGrantedSession(ctx, g.UserID, "sess_", "mcp_access", g.ClientID, mcpAccessTokenTTL, g.ID)
	if err != nil {
		return out, err
	}
	var refresh domain.Session
	// PG rolls the whole identity transaction back. Development FS cannot do so;
	// never leave partially issued bearers usable when a later write fails.
	defer func() {
		if err != nil {
			_ = s.repositories.DeleteSession(ctx, domain.HashToken(access.Token))
			if refresh.Token != "" {
				_ = s.repositories.DeleteSession(ctx, domain.HashToken(refresh.Token))
			}
		}
	}()
	refresh, err = s.issueGrantedSession(ctx, g.UserID, "refresh_", "mcp_refresh", g.ClientID, mcpRefreshTokenTTL, g.ID)
	if err != nil {
		return out, err
	}
	err = st.CreateOAuthRefreshReference(ctx, domain.OAuthRefreshReference{TokenHash: domain.HashToken(refresh.Token), GrantID: g.ID, ExpiresAt: refresh.ExpiresAt})
	if err != nil {
		return out, err
	}
	g.ExpiresAt = refresh.ExpiresAt
	if err = st.UpdateOAuthGrant(ctx, g); err != nil {
		return out, err
	}
	return domain.OAuthTokenPair{GrantID: g.ID, AccessToken: access.Token, RefreshToken: refresh.Token, ExpiresIn: int(mcpAccessTokenTTL.Seconds()), Scope: "mcp:read"}, nil
}

func (s *IdentityService) checkMCPGrant(ctx context.Context, sess domain.Session) error {
	if sess.GrantID == "" {
		return nil
	} // Unassociated legacy access expires naturally.
	st, err := s.grantStore()
	if err != nil {
		return err
	}
	g, err := st.GetOAuthGrant(ctx, sess.GrantID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrUnauthorized
	}
	if err != nil {
		return err
	}
	if g.UserID != sess.UserID || g.ClientID != sess.Label || !g.Active(time.Now().UTC()) {
		return domain.ErrUnauthorized
	}
	return nil
}

var errMCPGrantDenied = errors.New("MCP grant rejected")

// Refresh rejection after a detected replay must COMMIT the revocation before
// returning invalid_grant. An error from the transaction callback would undo it.
func (s *IdentityService) RefreshMCPAccessToken(ctx context.Context, token, client string) (domain.OAuthTokenPair, error) {
	var pair domain.OAuthTokenPair
	var rejected bool
	err := s.withIdentity(ctx, func(ctx context.Context) error {
		var err error
		pair, err = s.rotateMCPGrant(ctx, token, client)
		if errors.Is(err, errMCPGrantDenied) {
			rejected = true
			return nil
		}
		return err
	})
	if err != nil {
		return domain.OAuthTokenPair{}, err
	}
	if rejected {
		return domain.OAuthTokenPair{}, domain.ErrUnauthorized
	}
	return pair, nil
}

func (s *IdentityService) rotateMCPGrant(ctx context.Context, token, client string) (domain.OAuthTokenPair, error) {
	empty := domain.OAuthTokenPair{}
	if !strings.HasPrefix(token, "refresh_") || domain.ValidateExternalID(client) != nil {
		return empty, errMCPGrantDenied
	}
	st, err := s.grantStore()
	if err != nil {
		return empty, err
	}
	hash := domain.HashToken(token)
	ref, err := st.GetOAuthRefreshReference(ctx, hash)
	legacy := errors.Is(err, domain.ErrNotFound)
	if err != nil && !legacy {
		return empty, err
	}
	var g domain.OAuthGrant
	if !legacy {
		g, err = st.GetOAuthGrant(ctx, ref.GrantID)
		if errors.Is(err, domain.ErrNotFound) {
			return empty, errMCPGrantDenied
		}
		if err != nil {
			return empty, err
		}
		if g.ClientID != client || !g.Active(time.Now().UTC()) || !time.Now().Before(ref.ExpiresAt) {
			return empty, errMCPGrantDenied
		}
	}
	sess, err := s.repositories.GetSession(ctx, hash)
	if errors.Is(err, domain.ErrNotFound) && !legacy {
		if err = s.revokeGrant(ctx, st, g, "mcp.refresh_reused"); err != nil {
			return empty, err
		}
		return empty, errMCPGrantDenied
	}
	if errors.Is(err, domain.ErrNotFound) {
		return empty, errMCPGrantDenied
	}
	if err != nil {
		return empty, err
	}
	if sess.Kind != "mcp_refresh" || sess.Label != client || !time.Now().Before(sess.ExpiresAt) {
		return empty, errMCPGrantDenied
	}
	if legacy && sess.GrantID != "" {
		return empty, domain.ErrIntegrity
	} // Missing evidence is not legacy.
	if !legacy && (sess.GrantID != g.ID || sess.UserID != g.UserID) {
		return empty, domain.ErrIntegrity
	}
	if _, err = s.repositories.GetUser(ctx, sess.UserID); err != nil {
		return empty, errMCPGrantDenied
	}
	if legacy {
		now := time.Now().UTC().Truncate(time.Microsecond)
		g = domain.OAuthGrant{ID: domain.NewID("grt_"), UserID: sess.UserID, ClientID: client, CreatedAt: now, ExpiresAt: now.Add(mcpRefreshTokenTTL)}
		if err = st.CreateOAuthGrant(ctx, g); err != nil {
			return empty, err
		}
		// Attach just this proven refresh capability. Sibling tokens cannot be
		// inferred from client labels, issuance times or matching account emails.
		sess.GrantID = g.ID
		if err = s.repositories.CreateSession(ctx, sess); err != nil {
			return empty, err
		}
		if err = st.CreateOAuthRefreshReference(ctx, domain.OAuthRefreshReference{TokenHash: hash, GrantID: g.ID, ExpiresAt: sess.ExpiresAt}); err != nil {
			return empty, err
		}
	}
	if _, err = s.repositories.ConsumeSession(ctx, hash, "mcp_refresh", client); err != nil {
		return empty, err
	}
	pair, err := s.issueGrantPair(ctx, st, g)
	if err != nil {
		_ = s.repositories.CreateSession(ctx, sess)
		return empty, err
	}
	return pair, nil
}

func (s *IdentityService) revokeGrant(ctx context.Context, st outbound.OAuthGrants, g domain.OAuthGrant, action string) error {
	if g.RevokedAt != nil {
		return nil
	}
	audit, ok := s.repositories.(outbound.OAuthManagement)
	if !ok {
		return domain.ErrForbidden
	}
	now := time.Now().UTC()
	g.RevokedAt = &now
	if err := st.UpdateOAuthGrant(ctx, g); err != nil {
		return err
	}
	return audit.AppendAccountAudit(ctx, domain.AccountAuditEvent{ID: domain.NewID("aud_"), UserID: g.UserID, ClientID: g.ClientID, GrantID: g.ID, Action: action, CreatedAt: now})
}

// RFC 7009 revocation invalidates this authorization, including related access
// tokens. Invalid or foreign-client capabilities remain idempotent no-ops.
func (s *IdentityService) RevokeMCPToken(ctx context.Context, token, client string) error {
	if client == "" {
		return domain.ErrUnauthorized
	}
	return s.withIdentity(ctx, func(ctx context.Context) error {
		st, err := s.grantStore()
		if err != nil {
			return err
		}
		hash := domain.HashToken(token)
		var grant string
		switch {
		case strings.HasPrefix(token, "refresh_"):
			ref, err := st.GetOAuthRefreshReference(ctx, hash)
			if err == nil {
				grant = ref.GrantID
			} else if !errors.Is(err, domain.ErrNotFound) {
				return err
			}
		case strings.HasPrefix(token, "sess_"):
		default:
			return nil
		}
		if grant == "" {
			sess, err := s.repositories.GetSession(ctx, hash)
			if errors.Is(err, domain.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if sess.Label != client || (sess.Kind != "mcp_access" && sess.Kind != "mcp_refresh") {
				return nil
			}
			grant = sess.GrantID
			if grant == "" {
				return s.repositories.DeleteSession(ctx, hash)
			}
		}
		g, err := st.GetOAuthGrant(ctx, grant)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if g.ClientID != client {
			return nil
		}
		return s.revokeGrant(ctx, st, g, "mcp.grant_revoked")
	})
}
