package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type OAuthManagement interface {
	DeleteOAuthClientCodes(context.Context, string, string) error
	AppendAccountAudit(context.Context, domain.AccountAuditEvent) error
	ListAccountAudit(context.Context, string) ([]domain.AccountAuditEvent, error)
}
