//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.FederationStore = (*PostgresStore)(nil)

func (s *PostgresStore) requireIdentityTx(ctx context.Context) error {
	tx, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || tx.owner != s || !tx.identity || tx.readOnly {
		return domain.ErrConflict
	}
	return nil
}
func (s *PostgresStore) GetOIDCConnection(ctx context.Context, id string) (d domain.OIDCConnection, err error) {
	if domain.ValidateEnterpriseID(id) != nil {
		return d, domain.ErrValidation
	}
	err = s.db(ctx).QueryRow(ctx, `SELECT record,sealed_secret FROM enterprise_oidc_connections WHERE enterprise_id=$1`, id).Scan(&d, &d.Secret)
	if err != nil {
		return d, mapNoRows(err)
	}
	if d.EnterpriseID != id || d.Revision == "" {
		return d, domain.ErrIntegrity
	}
	return d, nil
}
func (s *PostgresStore) PutOIDCConnection(ctx context.Context, d domain.OIDCConnection) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if domain.ValidateEnterpriseID(d.EnterpriseID) != nil || d.Secret == "" || d.Revision == "" {
		return domain.ErrValidation
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO enterprise_oidc_connections(enterprise_id,record,sealed_secret) VALUES($1,$2,$3) ON CONFLICT(enterprise_id) DO UPDATE SET record=$2,sealed_secret=$3`, d.EnterpriseID, b, d.Secret)
	return mapPGConstraint(err)
}
func (s *PostgresStore) DeleteOIDCConnection(ctx context.Context, id string) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM enterprise_oidc_connections WHERE enterprise_id=$1`, id)
	return err
}
func (s *PostgresStore) CreateOIDCAttempt(ctx context.Context, a domain.OIDCAttempt) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if domain.ValidateEnterpriseID(a.EnterpriseID) != nil || a.Hash == "" || a.SessionHash == "" || a.ExpiresAt.IsZero() {
		return domain.ErrValidation
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO enterprise_oidc_attempts(hash,enterprise_id,record,expires_at) VALUES($1,$2,$3,$4)`, a.Hash, a.EnterpriseID, b, a.ExpiresAt)
	return mapPGConstraint(err)
}
func (s *PostgresStore) GetOIDCAttempt(ctx context.Context, hash string) (a domain.OIDCAttempt, err error) {
	var consumed bool
	err = s.db(ctx).QueryRow(ctx, `SELECT record,consumed FROM enterprise_oidc_attempts WHERE hash=$1`, hash).Scan(&a, &consumed)
	if err != nil {
		return a, mapNoRows(err)
	}
	if a.Hash != hash {
		return a, domain.ErrIntegrity
	}
	a.Consumed = consumed
	return a, nil
}
func (s *PostgresStore) ConsumeOIDCAttempt(ctx context.Context, hash string) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	r, err := s.db(ctx).Exec(ctx, `UPDATE enterprise_oidc_attempts SET consumed=true WHERE hash=$1 AND NOT consumed`, hash)
	if err != nil {
		return err
	}
	if r.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}
func (s *PostgresStore) LockFederationSession(ctx context.Context, hash string) (domain.Session, error) {
	if err := s.requireIdentityTx(ctx); err != nil {
		return domain.Session{}, err
	}
	var locked string
	if err := s.db(ctx).QueryRow(ctx, `SELECT token FROM sessions WHERE token=$1 FOR UPDATE`, hash).Scan(&locked); err != nil {
		return domain.Session{}, mapNoRows(err)
	}
	return s.GetSession(ctx, hash)
}
func (s *PostgresStore) GetFederationIdentity(ctx context.Context, ep, protocol, user string) (d domain.FederationIdentity, err error) {
	err = s.db(ctx).QueryRow(ctx, `SELECT record FROM enterprise_federation_identities WHERE enterprise_id=$1 AND protocol=$2 AND user_id=$3`, ep, protocol, user).Scan(&d)
	if err != nil {
		return d, mapNoRows(err)
	}
	if d.EnterpriseID != ep || d.Protocol != protocol || d.UserID != user {
		return d, domain.ErrIntegrity
	}
	return d, nil
}
func (s *PostgresStore) PutFederationIdentity(ctx context.Context, d domain.FederationIdentity) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if domain.ValidateEnterpriseID(d.EnterpriseID) != nil || domain.ValidateExternalID(d.UserID) != nil || (d.Protocol != "oidc" && d.Protocol != "saml") || d.Issuer == "" || d.Subject == "" {
		return domain.ErrValidation
	}
	old, err := s.GetFederationIdentity(ctx, d.EnterpriseID, d.Protocol, d.UserID)
	if err == nil {
		if old.Issuer == d.Issuer && old.Subject == d.Subject {
			return nil
		}
		return domain.ErrConflict
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO enterprise_federation_identities(enterprise_id,protocol,issuer,subject,user_id,record) VALUES($1,$2,$3,$4,$5,$6)`, d.EnterpriseID, d.Protocol, d.Issuer, d.Subject, d.UserID, b)
	return mapPGConstraint(err)
}
func (s *PostgresStore) GetFederationSession(ctx context.Context, ep, hash string) (d domain.FederationSession, err error) {
	err = s.db(ctx).QueryRow(ctx, `SELECT record FROM enterprise_federation_sessions WHERE enterprise_id=$1 AND session_hash=$2`, ep, hash).Scan(&d)
	if err != nil {
		return d, mapNoRows(err)
	}
	if d.EnterpriseID != ep || d.SessionHash != hash {
		return d, domain.ErrIntegrity
	}
	return d, nil
}
func (s *PostgresStore) PutFederationSession(ctx context.Context, d domain.FederationSession) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if domain.ValidateEnterpriseID(d.EnterpriseID) != nil || d.SessionHash == "" || d.ConnectionRevision == "" || d.UserID == "" || (d.Protocol != "oidc" && d.Protocol != "saml") || !d.ExpiresAt.After(time.Now()) {
		return domain.ErrValidation
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO enterprise_federation_sessions(enterprise_id,session_hash,record,expires_at) VALUES($1,$2,$3,$4) ON CONFLICT(enterprise_id,session_hash) DO UPDATE SET record=$3,expires_at=$4`, d.EnterpriseID, d.SessionHash, b, d.ExpiresAt)
	return err
}
