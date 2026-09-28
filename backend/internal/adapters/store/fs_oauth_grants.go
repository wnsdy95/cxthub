package store

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.OAuthGrants = (*FSStore)(nil)

func (s *FSStore) oauthGrantPath(id string) string {
	return oauthRecordPath(filepath.Join(s.dataDir, "oauth", "grants"), id)
}
func (s *FSStore) oauthRefreshPath(hash string) string {
	return oauthRecordPath(filepath.Join(s.dataDir, "oauth", "refresh-references"), hash)
}
func (s *FSStore) CreateOAuthGrant(ctx context.Context, g domain.OAuthGrant) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := domain.ValidateOAuthGrant(g); err != nil {
		return err
	}
	return writeExclusiveJSON(s.oauthGrantPath(g.ID), g)
}
func (s *FSStore) GetOAuthGrant(ctx context.Context, id string) (g domain.OAuthGrant, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if domain.ValidateExternalID(id) != nil {
		return g, domain.ErrValidation
	}
	err = readJSON(s.oauthGrantPath(id), &g)
	if err != nil {
		return
	}
	if g.ID != id || domain.ValidateOAuthGrant(g) != nil {
		return g, domain.ErrIntegrity
	}
	return
}
func (s *FSStore) UpdateOAuthGrant(ctx context.Context, g domain.OAuthGrant) error {
	if err := domain.ValidateOAuthGrant(g); err != nil {
		return err
	}
	old, err := s.GetOAuthGrant(ctx, g.ID)
	if err != nil {
		return err
	}
	if old.UserID != g.UserID || old.ClientID != g.ClientID || !old.CreatedAt.Equal(g.CreatedAt) || old.RevokedAt != nil {
		return domain.ErrConflict
	}
	b, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return writeAtomic(s.oauthGrantPath(g.ID), b)
}
func (s *FSStore) CreateOAuthRefreshReference(ctx context.Context, r domain.OAuthRefreshReference) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := domain.ValidateOAuthRefreshReference(r); err != nil {
		return err
	}
	return writeExclusiveJSON(s.oauthRefreshPath(r.TokenHash), r)
}
func (s *FSStore) GetOAuthRefreshReference(ctx context.Context, hash string) (r domain.OAuthRefreshReference, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if domain.ValidateStoredSessionToken(hash) != nil {
		return r, domain.ErrValidation
	}
	err = readJSON(s.oauthRefreshPath(hash), &r)
	if err != nil {
		return
	}
	if r.TokenHash != hash || domain.ValidateOAuthRefreshReference(r) != nil {
		return r, domain.ErrIntegrity
	}
	return
}
