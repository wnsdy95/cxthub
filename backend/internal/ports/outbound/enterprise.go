package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type EnterpriseStore interface {
	CreateEnterprise(context.Context, domain.Enterprise, domain.EnterpriseMembership) error
	GetEnterprise(context.Context, string) (domain.Enterprise, error)
	GetEnterpriseBySlug(context.Context, string) (domain.Enterprise, error)
	ListEnterprisesForUser(context.Context, string) ([]domain.Enterprise, error)
	UpdateEnterprise(context.Context, domain.Enterprise) error
	ListEnterpriseMembers(context.Context, string) ([]domain.EnterpriseMembership, error)
	PutEnterpriseMember(context.Context, domain.EnterpriseMembership) error
	RemoveEnterpriseMember(context.Context, string, string) error
	ListEnterpriseOrganizations(context.Context, string) ([]domain.Organization, error)
	OrganizationEnterprise(context.Context, string) (domain.Enterprise, error)
	SetOrganizationEnterprise(context.Context, string, string) error
	AppendEnterpriseAudit(context.Context, domain.EnterpriseAuditEvent) error
	ListEnterpriseAudit(context.Context, string, int) ([]domain.EnterpriseAuditEvent, error)
}
