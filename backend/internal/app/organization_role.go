package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func organizationRoleFromReader(ctx context.Context, organizations interface {
	GetOrganizationMembership(context.Context, string, string) (domain.OrganizationMembership, error)
}, organizationID, userID string) (domain.OrganizationRole, bool) {
	if organizations == nil || userID == "" {
		return "", false
	}
	member, err := organizations.GetOrganizationMembership(ctx, organizationID, userID)
	if err != nil || !domain.ValidOrganizationRole(member.Role) {
		return "", false
	}
	return member.Role, true
}
