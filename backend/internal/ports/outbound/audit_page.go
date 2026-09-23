package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type OrganizationAuditReader interface {
	OrganizationAuditBefore(context.Context, domain.AuditCursor, int) ([]domain.OrganizationAuditEvent, error)
}
