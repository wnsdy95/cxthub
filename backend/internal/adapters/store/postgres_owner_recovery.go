//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.OwnerRecoveryStore = (*PostgresStore)(nil)

func (s *PostgresStore) GetOwnerRecovery(ctx context.Context, ep, user string) (r domain.OwnerRecovery, err error) {
	if domain.ValidateEnterpriseID(ep) != nil || domain.ValidateExternalID(user) != nil {
		return r, domain.ErrValidation
	}
	err = s.db(ctx).QueryRow(ctx, `SELECT record FROM enterprise_owner_recovery WHERE enterprise_id=$1 AND user_id=$2`, ep, user).Scan(&r)
	if errors.Is(mapNoRows(err), domain.ErrNotFound) {
		return domain.DefaultOwnerRecovery(ep, user), nil
	}
	if err != nil {
		return r, err
	}
	if r.EnterpriseID != ep || r.UserID != user || r.Validate() != nil {
		return r, domain.ErrIntegrity
	}
	return r, nil
}
func (s *PostgresStore) PutOwnerRecovery(ctx context.Context, r domain.OwnerRecovery) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO enterprise_owner_recovery(enterprise_id,user_id,record) VALUES($1,$2,$3) ON CONFLICT(enterprise_id,user_id) DO UPDATE SET record=$3`, r.EnterpriseID, r.UserID, b)
	return mapPGConstraint(err)
}
func (s *PostgresStore) GetOwnerRepairSession(ctx context.Context, ep, hash string) (r domain.OwnerRepairSession, err error) {
	if domain.ValidateEnterpriseID(ep) != nil {
		return r, domain.ErrValidation
	}
	err = s.db(ctx).QueryRow(ctx, `SELECT record FROM enterprise_owner_repair_sessions WHERE enterprise_id=$1 AND session_hash=$2`, ep, hash).Scan(&r)
	if err != nil {
		return r, mapNoRows(err)
	}
	if r.EnterpriseID != ep || r.SessionHash != hash || r.Validate() != nil {
		return r, domain.ErrIntegrity
	}
	return r, nil
}
func (s *PostgresStore) PutOwnerRepairSession(ctx context.Context, r domain.OwnerRepairSession) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO enterprise_owner_repair_sessions(enterprise_id,session_hash,record) VALUES($1,$2,$3) ON CONFLICT(enterprise_id,session_hash) DO UPDATE SET record=$3`, r.EnterpriseID, r.SessionHash, b)
	return mapPGConstraint(err)
}
