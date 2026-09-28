//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.EnterpriseDomainStore = (*PostgresStore)(nil)

func (s *PostgresStore) ListEnterpriseDomains(ctx context.Context, id string) ([]domain.EnterpriseDomain, error) {
	if err := domain.ValidateEnterpriseID(id); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT record FROM enterprise_domains WHERE enterprise_id=$1 ORDER BY domain`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.EnterpriseDomain{}
	for rows.Next() {
		var d domain.EnterpriseDomain
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		if d.EnterpriseID != id || d.Validate() != nil {
			return nil, domain.ErrIntegrity
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *PostgresStore) GetEnterpriseDomain(ctx context.Context, id, name string) (d domain.EnterpriseDomain, err error) {
	if domain.ValidateEnterpriseID(id) != nil {
		return d, domain.ErrValidation
	}
	err = s.db(ctx).QueryRow(ctx, `SELECT record FROM enterprise_domains WHERE enterprise_id=$1 AND domain=$2`, id, name).Scan(&d)
	if err != nil {
		return d, mapNoRows(err)
	}
	if d.EnterpriseID != id || d.Domain != name || d.Validate() != nil {
		return d, domain.ErrIntegrity
	}
	return d, nil
}
func (s *PostgresStore) PutEnterpriseDomain(ctx context.Context, d domain.EnterpriseDomain) error {
	if err := d.Validate(); err != nil {
		return err
	}
	// Only identity transactions may change globally exclusive claims. This
	// shares the lock used for owner revocation and audit rollback.
	tx, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || tx.owner != s || !tx.identity || tx.readOnly {
		return domain.ErrConflict
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO enterprise_domains(enterprise_id,domain,record,verified_until) VALUES($1,$2,$3,$4) ON CONFLICT(enterprise_id,domain) DO UPDATE SET record=$3,verified_until=$4`, d.EnterpriseID, d.Domain, b, d.VerifiedUntil)
	return mapPGConstraint(err)
}
func (s *PostgresStore) RemoveEnterpriseDomain(ctx context.Context, id, name string) error {
	tx, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || tx.owner != s || !tx.identity || tx.readOnly {
		return domain.ErrConflict
	}
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM enterprise_domains WHERE enterprise_id=$1 AND domain=$2`, id, name)
	return err
}
func (s *PostgresStore) OtherVerifiedEnterpriseDomain(ctx context.Context, name, id string, now time.Time) (bool, error) {
	var exists bool
	err := s.db(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM enterprise_domains WHERE domain=$1 AND enterprise_id<>$2 AND verified_until>$3)`, name, id, now).Scan(&exists)
	return exists, err
}
