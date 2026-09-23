//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) OrganizationAuditBefore(ctx context.Context, c domain.AuditCursor, limit int) ([]domain.OrganizationAuditEvent, error) {
	if domain.ValidateOrganizationID(c.OrganizationID) != nil || limit < 1 || limit > 501 {
		return nil, domain.ErrValidation
	}
	var before any
	if !c.Before.IsZero() {
		before = c.Before
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT id,organization_id,actor_id,action,target_type,target_id,reason,created_at,correlation_id FROM organization_audit_events WHERE organization_id=$1 AND ($2::timestamptz IS NULL OR (created_at,id)<($2::timestamptz,$3)) ORDER BY created_at DESC,id DESC LIMIT $4`, c.OrganizationID, before, c.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.OrganizationAuditEvent{}
	for rows.Next() {
		var e domain.OrganizationAuditEvent
		if err = rows.Scan(&e.ID, &e.OrganizationID, &e.ActorID, &e.Action, &e.TargetType, &e.TargetID, &e.Reason, &e.CreatedAt, &e.CorrelationID); err != nil {
			return nil, err
		}
		if err = domain.ValidateOrganizationAuditEvent(e); err != nil {
			return nil, storedIdentityIntegrity(err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
