package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *FSStore) OrganizationAuditBefore(ctx context.Context, c domain.AuditCursor, limit int) ([]domain.OrganizationAuditEvent, error) {
	if domain.ValidateOrganizationID(c.OrganizationID) != nil || limit < 1 || limit > 501 {
		return nil, domain.ErrValidation
	}
	dir := filepath.Join(s.organizationAuditDir(), c.OrganizationID)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []domain.OrganizationAuditEvent{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []domain.OrganizationAuditEvent{}
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		entry := entries[i]
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var e domain.OrganizationAuditEvent
		if err = readJSON(filepath.Join(dir, entry.Name()), &e); err != nil {
			return nil, err
		}
		if domain.ValidateOrganizationAuditEvent(e) != nil || e.OrganizationID != c.OrganizationID {
			return nil, domain.ErrIntegrity
		}
		if !c.Before.IsZero() && (e.CreatedAt.After(c.Before) || (e.CreatedAt.Equal(c.Before) && e.ID >= c.ID)) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
