package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type InvitationReader interface {
	GetCollaborationInvite(context.Context, string) (domain.CollaborationInvite, error)
}

type InvitationOrganizationReader interface {
	GetOrganizationMembership(context.Context, string, string) (domain.OrganizationMembership, error)
}

type InvitationEnterpriseReader interface {
	ListEnterpriseMembers(context.Context, string) ([]domain.EnterpriseMembership, error)
}

type invitationAuthority struct {
	organizations InvitationOrganizationReader
	enterprises   InvitationEnterpriseReader
}

func (authority invitationAuthority) authorizeInvitation(ctx context.Context, actor, kind, id string, target domain.OrganizationRole) error {
	if err := domain.ValidateCollaborationScope(kind, id); err != nil {
		return err
	}
	// Preserve storage failures; a worker must not treat an unavailable
	// membership store as a permanent revocation.
	var role domain.OrganizationRole
	if kind == "organization" {
		if authority.organizations == nil {
			return domain.ErrForbidden
		}
		member, err := authority.organizations.GetOrganizationMembership(ctx, id, actor)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrForbidden
		}
		if err != nil {
			return err
		}
		role = member.Role
	} else {
		if authority.enterprises == nil {
			return domain.ErrForbidden
		}
		members, err := authority.enterprises.ListEnterpriseMembers(ctx, id)
		if err != nil {
			return err
		}
		for _, member := range members {
			if member.UserID == actor {
				role = domain.OrganizationRole(member.Role)
				break
			}
		}
	}
	if !role.AtLeast(domain.OrganizationAdmin) || (target != domain.OrganizationMember && role != domain.OrganizationOwner) {
		return domain.ErrForbidden
	}
	return nil
}
