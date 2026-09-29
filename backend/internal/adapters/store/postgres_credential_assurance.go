//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.CredentialAssurances = (*PostgresStore)(nil)

func (s *PostgresStore) GetAssurancePolicy(ctx context.Context, id string) (p domain.AssurancePolicy, err error) {
	if domain.ValidateEnterpriseID(id) != nil {
		return p, domain.ErrValidation
	}
	err = s.db(ctx).QueryRow(ctx, `SELECT record FROM enterprise_assurance_policies WHERE enterprise_id=$1`, id).Scan(&p)
	if errors.Is(mapNoRows(err), domain.ErrNotFound) {
		return domain.DefaultAssurancePolicy(id), nil
	}
	if err != nil {
		return p, err
	}
	if p.EnterpriseID != id || p.Validate() != nil {
		return p, domain.ErrIntegrity
	}
	return p, nil
}
func (s *PostgresStore) PutAssurancePolicy(ctx context.Context, p domain.AssurancePolicy) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO enterprise_assurance_policies(enterprise_id,record) VALUES($1,$2) ON CONFLICT(enterprise_id) DO UPDATE SET record=$2`, p.EnterpriseID, b)
	return mapPGConstraint(err)
}
func (s *PostgresStore) ListCredentialAssurances(ctx context.Context, ep, user string) ([]domain.CredentialAssurance, error) {
	if domain.ValidateEnterpriseID(ep) != nil || domain.ValidateExternalID(user) != nil {
		return nil, domain.ErrValidation
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT credential_id,record FROM enterprise_credential_assurances WHERE enterprise_id=$1 AND user_id=$2`, ep, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.CredentialAssurance{}
	for rows.Next() {
		var id string
		var a domain.CredentialAssurance
		if err := rows.Scan(&id, &a); err != nil {
			return nil, err
		}
		if a.EnterpriseID != ep || a.CredentialID != id || a.UserID != user || a.Validate() != nil {
			return nil, domain.ErrIntegrity
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *PostgresStore) PutCredentialAssurance(ctx context.Context, a domain.CredentialAssurance) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if err := a.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	r, err := s.db(ctx).Exec(ctx, `INSERT INTO enterprise_credential_assurances(enterprise_id,credential_id,user_id,record) VALUES($1,$2,$3,$4) ON CONFLICT(enterprise_id,credential_id) DO UPDATE SET record=$4 WHERE enterprise_credential_assurances.user_id=$3`, a.EnterpriseID, a.CredentialID, a.UserID, b)
	if err != nil {
		return mapPGConstraint(err)
	}
	if r.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}
