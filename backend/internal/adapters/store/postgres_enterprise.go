//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.EnterpriseStore = (*PostgresStore)(nil)

func (s *PostgresStore) CreateEnterprise(ctx context.Context, e domain.Enterprise, owner domain.EnterpriseMembership) error {
	if err := domain.ValidateEnterprise(e); err != nil {
		return err
	}
	if err := domain.ValidateEnterpriseMembership(owner); err != nil {
		return err
	}
	if owner.EnterpriseID != e.ID || owner.Role != domain.EnterpriseOwner {
		return domain.ErrValidation
	}
	return s.WithinIdentity(ctx, func(ctx context.Context) error {
		if _, err := s.GetEnterpriseBySlug(ctx, e.Slug); err == nil {
			return domain.ErrConflict
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err = s.db(ctx).Exec(ctx, `INSERT INTO enterprises(id,slug,record) VALUES($1,$2,$3)`, e.ID, e.Slug, data); err != nil {
			return mapPGConstraint(err)
		}
		return s.PutEnterpriseMember(ctx, owner)
	})
}
func (s *PostgresStore) GetEnterprise(ctx context.Context, id string) (domain.Enterprise, error) {
	if err := domain.ValidateEnterpriseID(id); err != nil {
		return domain.Enterprise{}, err
	}
	var e domain.Enterprise
	err := s.db(ctx).QueryRow(ctx, `SELECT record FROM enterprises WHERE id=$1`, id).Scan(&e)
	if err != nil {
		return e, mapNoRows(err)
	}
	if e.ID != id {
		return e, domain.ErrIntegrity
	}
	return e, domain.ValidateEnterprise(e)
}
func (s *PostgresStore) GetEnterpriseBySlug(ctx context.Context, slug string) (domain.Enterprise, error) {
	if !domain.ValidNamespaceSlug(slug) {
		return domain.Enterprise{}, domain.ErrValidation
	}
	var id string
	if err := s.db(ctx).QueryRow(ctx, `SELECT id FROM enterprises WHERE slug=$1 UNION ALL SELECT enterprise_id FROM enterprise_slug_aliases WHERE slug=$1 LIMIT 1`, slug).Scan(&id); err != nil {
		return domain.Enterprise{}, mapNoRows(err)
	}
	return s.GetEnterprise(ctx, id)
}
func (s *PostgresStore) ListEnterprisesForUser(ctx context.Context, user string) ([]domain.Enterprise, error) {
	if err := domain.ValidateExternalID(user); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT record FROM enterprises e WHERE EXISTS(SELECT 1 FROM enterprise_memberships m WHERE m.enterprise_id=e.id AND m.user_id=$1) OR EXISTS(SELECT 1 FROM enterprise_organizations eo JOIN organization_memberships om ON om.organization_id=eo.organization_id WHERE eo.enterprise_id=e.id AND om.user_id=$1) ORDER BY slug`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Enterprise{}
	for rows.Next() {
		var e domain.Enterprise
		if err = rows.Scan(&e); err != nil {
			return nil, err
		}
		if err = domain.ValidateEnterprise(e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *PostgresStore) UpdateEnterprise(ctx context.Context, e domain.Enterprise) error {
	if err := domain.ValidateEnterprise(e); err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	result, err := s.db(ctx).Exec(ctx, `UPDATE enterprises SET record=$2 WHERE id=$1 AND slug=$3`, e.ID, data, e.Slug)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}
func (s *PostgresStore) ListEnterpriseMembers(ctx context.Context, id string) ([]domain.EnterpriseMembership, error) {
	if err := domain.ValidateEnterpriseID(id); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT enterprise_id,user_id,role,created_at FROM enterprise_memberships WHERE enterprise_id=$1 ORDER BY user_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.EnterpriseMembership{}
	for rows.Next() {
		var m domain.EnterpriseMembership
		if err = rows.Scan(&m.EnterpriseID, &m.UserID, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *PostgresStore) PutEnterpriseMember(ctx context.Context, m domain.EnterpriseMembership) error {
	if err := domain.ValidateEnterpriseMembership(m); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO enterprise_memberships(enterprise_id,user_id,role,created_at) VALUES($1,$2,$3,$4) ON CONFLICT(enterprise_id,user_id) DO UPDATE SET role=EXCLUDED.role`, m.EnterpriseID, m.UserID, m.Role, m.CreatedAt)
	return mapPGConstraint(err)
}
func (s *PostgresStore) RemoveEnterpriseMember(ctx context.Context, id, user string) error {
	if err := domain.ValidateEnterpriseID(id); err != nil {
		return err
	}
	if err := domain.ValidateExternalID(user); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM enterprise_memberships WHERE enterprise_id=$1 AND user_id=$2`, id, user)
	return mapPGConstraint(err)
}
func (s *PostgresStore) ListEnterpriseOrganizations(ctx context.Context, id string) ([]domain.Organization, error) {
	if err := domain.ValidateEnterpriseID(id); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT organization_id FROM enterprise_organizations WHERE enterprise_id=$1 ORDER BY organization_id`, id)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := []domain.Organization{}
	for _, id := range ids {
		o, err := s.GetOrganization(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}
func (s *PostgresStore) OrganizationEnterprise(ctx context.Context, org string) (domain.Enterprise, error) {
	if err := domain.ValidateOrganizationID(org); err != nil {
		return domain.Enterprise{}, err
	}
	var id string
	if err := s.db(ctx).QueryRow(ctx, `SELECT enterprise_id FROM enterprise_organizations WHERE organization_id=$1`, org).Scan(&id); err != nil {
		return domain.Enterprise{}, mapNoRows(err)
	}
	return s.GetEnterprise(ctx, id)
}
func (s *PostgresStore) SetOrganizationEnterprise(ctx context.Context, org, id string) error {
	if err := domain.ValidateOrganizationID(org); err != nil {
		return err
	}
	if id == "" {
		_, err := s.db(ctx).Exec(ctx, `DELETE FROM enterprise_organizations WHERE organization_id=$1`, org)
		return err
	}
	if err := domain.ValidateEnterpriseID(id); err != nil {
		return err
	}
	result, err := s.db(ctx).Exec(ctx, `INSERT INTO enterprise_organizations(organization_id,enterprise_id) VALUES($1,$2) ON CONFLICT(organization_id) DO UPDATE SET enterprise_id=EXCLUDED.enterprise_id WHERE enterprise_organizations.enterprise_id=EXCLUDED.enterprise_id`, org, id)
	if err != nil {
		return mapPGConstraint(err)
	}
	if result.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}
func (s *PostgresStore) AppendEnterpriseAudit(ctx context.Context, e domain.EnterpriseAuditEvent) error {
	if err := domain.ValidateEnterpriseID(e.EnterpriseID); err != nil {
		return err
	}
	if err := domain.ValidateAuditID(e.ID); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO enterprise_audit_events(id,enterprise_id,actor_id,action,target_id,created_at) VALUES($1,$2,$3,$4,$5,$6)`, e.ID, e.EnterpriseID, e.ActorID, e.Action, e.TargetID, e.CreatedAt)
	return err
}
func (s *PostgresStore) ListEnterpriseAudit(ctx context.Context, id string, limit int) ([]domain.EnterpriseAuditEvent, error) {
	if err := domain.ValidateEnterpriseID(id); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT id,enterprise_id,actor_id,action,target_id,created_at FROM enterprise_audit_events WHERE enterprise_id=$1 ORDER BY created_at DESC,id DESC LIMIT $2`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.EnterpriseAuditEvent{}
	for rows.Next() {
		var e domain.EnterpriseAuditEvent
		if err = rows.Scan(&e.ID, &e.EnterpriseID, &e.ActorID, &e.Action, &e.TargetID, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
