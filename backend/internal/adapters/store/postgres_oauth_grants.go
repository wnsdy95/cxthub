//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.OAuthGrants = (*PostgresStore)(nil)

func (s *PostgresStore) CreateOAuthGrant(ctx context.Context, g domain.OAuthGrant) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if err := domain.ValidateOAuthGrant(g); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO oauth_grants(id,user_id,client_id,created_at,expires_at,revoked_at) VALUES($1,$2,$3,$4,$5,$6)`, g.ID, g.UserID, g.ClientID, g.CreatedAt, g.ExpiresAt, g.RevokedAt)
	return mapPGConstraint(err)
}
func (s *PostgresStore) GetOAuthGrant(ctx context.Context, id string) (g domain.OAuthGrant, err error) {
	if domain.ValidateExternalID(id) != nil {
		return g, domain.ErrValidation
	}
	err = s.db(ctx).QueryRow(ctx, `SELECT id,user_id,client_id,created_at,expires_at,revoked_at FROM oauth_grants WHERE id=$1`, id).Scan(&g.ID, &g.UserID, &g.ClientID, &g.CreatedAt, &g.ExpiresAt, &g.RevokedAt)
	if err != nil {
		return g, mapNoRows(err)
	}
	if g.ID != id || domain.ValidateOAuthGrant(g) != nil {
		return g, domain.ErrIntegrity
	}
	return g, nil
}
func (s *PostgresStore) UpdateOAuthGrant(ctx context.Context, g domain.OAuthGrant) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if err := domain.ValidateOAuthGrant(g); err != nil {
		return err
	}
	r, err := s.db(ctx).Exec(ctx, `UPDATE oauth_grants SET expires_at=$5,revoked_at=$6 WHERE id=$1 AND user_id=$2 AND client_id=$3 AND created_at=$4 AND revoked_at IS NULL`, g.ID, g.UserID, g.ClientID, g.CreatedAt, g.ExpiresAt, g.RevokedAt)
	if err != nil {
		return err
	}
	if r.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}
func (s *PostgresStore) CreateOAuthRefreshReference(ctx context.Context, r domain.OAuthRefreshReference) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if err := domain.ValidateOAuthRefreshReference(r); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO oauth_refresh_references(token_hash,grant_id,expires_at) VALUES($1,$2,$3)`, r.TokenHash, r.GrantID, r.ExpiresAt)
	return mapPGConstraint(err)
}
func (s *PostgresStore) GetOAuthRefreshReference(ctx context.Context, hash string) (r domain.OAuthRefreshReference, err error) {
	if domain.ValidateStoredSessionToken(hash) != nil {
		return r, domain.ErrValidation
	}
	err = s.db(ctx).QueryRow(ctx, `SELECT token_hash,grant_id,expires_at FROM oauth_refresh_references WHERE token_hash=$1`, hash).Scan(&r.TokenHash, &r.GrantID, &r.ExpiresAt)
	if err != nil {
		return r, mapNoRows(err)
	}
	if r.TokenHash != hash || domain.ValidateOAuthRefreshReference(r) != nil {
		return r, domain.ErrIntegrity
	}
	return r, nil
}
