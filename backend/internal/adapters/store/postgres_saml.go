//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.SAMLStore = (*PostgresStore)(nil)

func (s *PostgresStore) GetSAMLConnection(ctx context.Context, id string) (d domain.SAMLConnection, err error) {
	if domain.ValidateEnterpriseID(id) != nil {
		return d, domain.ErrValidation
	}
	err = s.db(ctx).QueryRow(ctx, `SELECT record,metadata,sealed_key FROM enterprise_saml_connections WHERE enterprise_id=$1`, id).Scan(&d, &d.Metadata, &d.PrivateKey)
	if err != nil {
		return d, mapNoRows(err)
	}
	if d.EnterpriseID != id || d.Revision == "" {
		return d, domain.ErrIntegrity
	}
	return d, nil
}
func (s *PostgresStore) PutSAMLConnection(ctx context.Context, d domain.SAMLConnection) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if domain.ValidateEnterpriseID(d.EnterpriseID) != nil || d.Revision == "" || d.PrivateKey == "" || d.Metadata == "" {
		return domain.ErrValidation
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO enterprise_saml_connections(enterprise_id,record,metadata,sealed_key) VALUES($1,$2,$3,$4) ON CONFLICT(enterprise_id) DO UPDATE SET record=$2,metadata=$3,sealed_key=$4`, d.EnterpriseID, b, d.Metadata, d.PrivateKey)
	return mapPGConstraint(err)
}
func (s *PostgresStore) DeleteSAMLConnection(ctx context.Context, id string) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM enterprise_saml_connections WHERE enterprise_id=$1`, id)
	return err
}
func (s *PostgresStore) CreateSAMLAttempt(ctx context.Context, a domain.SAMLAttempt) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if a.Hash == "" || a.RequestID == "" || a.SessionHash == "" || a.Received || a.Completed || a.FinishHash != "" {
		return domain.ErrValidation
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO enterprise_saml_attempts(hash,enterprise_id,record,expires_at) VALUES($1,$2,$3,$4)`, a.Hash, a.EnterpriseID, b, a.ExpiresAt)
	return mapPGConstraint(err)
}
func (s *PostgresStore) GetSAMLAttempt(ctx context.Context, hash string) (a domain.SAMLAttempt, err error) {
	err = s.db(ctx).QueryRow(ctx, `SELECT record,received,completed FROM enterprise_saml_attempts WHERE hash=$1`, hash).Scan(&a, &a.Received, &a.Completed)
	if err != nil {
		return a, mapNoRows(err)
	}
	if a.Hash != hash {
		return a, domain.ErrIntegrity
	}
	return a, nil
}
func (s *PostgresStore) GetSAMLFinish(ctx context.Context, hash string) (a domain.SAMLAttempt, err error) {
	err = s.db(ctx).QueryRow(ctx, `SELECT record,received,completed FROM enterprise_saml_attempts WHERE finish_hash=$1`, hash).Scan(&a, &a.Received, &a.Completed)
	if err != nil {
		return a, mapNoRows(err)
	}
	if a.FinishHash != hash {
		return a, domain.ErrIntegrity
	}
	return a, nil
}
func (s *PostgresStore) ReceiveSAMLAttempt(ctx context.Context, a domain.SAMLAttempt) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if !a.Received || a.Completed || a.FinishHash == "" || a.AssertionID == "" || a.Proof.Protocol != "saml" {
		return domain.ErrValidation
	}
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO enterprise_saml_replays(enterprise_id,issuer,assertion_id,expires_at) VALUES($1,$2,$3,$4)`, a.EnterpriseID, a.Proof.Issuer, a.AssertionID, a.Proof.ExpiresAt)
	if err != nil {
		return mapPGConstraint(err)
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	r, err := s.db(ctx).Exec(ctx, `UPDATE enterprise_saml_attempts SET record=$2,received=true,finish_hash=$3,expires_at=$4 WHERE hash=$1 AND NOT received AND NOT completed`, a.Hash, b, a.FinishHash, a.ExpiresAt)
	if err != nil {
		return mapPGConstraint(err)
	}
	if r.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}
func (s *PostgresStore) FinishSAMLAttempt(ctx context.Context, hash string) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	r, err := s.db(ctx).Exec(ctx, `UPDATE enterprise_saml_attempts SET completed=true WHERE hash=$1 AND received AND NOT completed`, hash)
	if err != nil {
		return err
	}
	if r.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}
