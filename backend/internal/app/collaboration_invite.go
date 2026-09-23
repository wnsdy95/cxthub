package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type CollaborationInvitation struct {
	domain.CollaborationInvite
	SpaceName    string `json:"space_name"`
	SpacePath    string `json:"space_path"`
	EmailEnabled bool   `json:"email_enabled"`
	EmailStatus  string `json:"email_status"`
	EmailReason  string `json:"email_reason,omitempty"`
}

func (s *IdentityService) invitationStore() (outbound.CollaborationInviteStore, error) {
	st, ok := s.repositories.(outbound.CollaborationInviteStore)
	if !ok {
		return nil, domain.ErrForbidden
	}
	return st, nil
}
func (s *IdentityService) collaborationRole(ctx context.Context, actor, kind, id string) domain.OrganizationRole {
	if kind == "organization" {
		role, _ := s.OrganizationRoleOf(ctx, id, actor)
		return role
	}
	if kind == "enterprise" {
		role, _ := s.EnterpriseRoleOf(ctx, id, actor)
		return domain.OrganizationRole(role)
	}
	return ""
}
func (s *IdentityService) authorizeInvitation(ctx context.Context, actor, kind, id string, target domain.OrganizationRole) error {
	if err := domain.ValidateCollaborationScope(kind, id); err != nil {
		return err
	}
	// Preserve storage failures; a worker must not treat an unavailable
	// membership store as a permanent revocation.
	var role domain.OrganizationRole
	if kind == "organization" {
		if s.organization == nil {
			return domain.ErrForbidden
		}
		member, err := s.organization.GetOrganizationMembership(ctx, id, actor)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrForbidden
		}
		if err != nil {
			return err
		}
		role = member.Role
	} else {
		st, err := s.enterpriseStore()
		if err != nil {
			return err
		}
		members, err := st.ListEnterpriseMembers(ctx, id)
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
func (s *IdentityService) invitationAudit(ctx context.Context, i domain.CollaborationInvite, actor, action string) error {
	if i.Kind == "enterprise" {
		return s.enterpriseAudit(ctx, i.SpaceID, actor, "invitation."+action, i.ID)
	}
	return s.organization.AppendOrganizationAudit(ctx, organizationAudit(ctx, i.SpaceID, actor, "invitation."+action, "invitation", i.ID, "", time.Now().UTC()))
}
func (s *IdentityService) invitationView(ctx context.Context, i domain.CollaborationInvite) (CollaborationInvitation, error) {
	v := CollaborationInvitation{CollaborationInvite: i, EmailEnabled: s.invitationEmail != nil, EmailStatus: "not_sent"}
	if st, ok := s.repositories.(outbound.InvitationEmailStore); ok {
		j, err := st.GetInvitationEmail(ctx, i.ID)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return v, err
		}
		if err == nil {
			v.EmailStatus, v.EmailReason = j.State, j.Reason
		}
	}
	if i.Status == "pending" && !time.Now().Before(i.ExpiresAt) {
		v.Status = "expired"
	}
	if i.Kind == "organization" {
		org, err := s.organization.GetOrganization(ctx, i.SpaceID)
		if err != nil {
			return v, err
		}
		v.SpaceName, v.SpacePath = org.Name, "/"+org.Slug
	} else {
		st, err := s.enterpriseStore()
		if err != nil {
			return v, err
		}
		e, err := st.GetEnterprise(ctx, i.SpaceID)
		if err != nil {
			return v, err
		}
		v.SpaceName, v.SpacePath = e.Name, "/enterprises/"+e.Slug
	}
	return v, nil
}
func invitationRecipient(i domain.CollaborationInvite, u domain.User) bool {
	email, err := domain.NormalizeInvitationEmail(u.Email)
	return err == nil && email == i.Email && (i.RecipientID == "" || i.RecipientID == u.ID)
}

func (s *IdentityService) CreateCollaborationInvitation(ctx context.Context, actor, kind, space, recipient string, role domain.OrganizationRole, days int) (CollaborationInvitation, error) {
	return identityResult(ctx, s, func(ctx context.Context) (CollaborationInvitation, error) {
		if !domain.ValidOrganizationRole(role) || days < 1 || days > 30 {
			return CollaborationInvitation{}, domain.ErrValidation
		}
		if err := s.authorizeInvitation(ctx, actor, kind, space, role); err != nil {
			return CollaborationInvitation{}, err
		}
		st, err := s.invitationStore()
		if err != nil {
			return CollaborationInvitation{}, err
		}
		recipient = strings.TrimSpace(recipient)
		var recipientID string
		if strings.HasPrefix(recipient, "@") {
			id, err := s.memberSubject(ctx, recipient)
			if err != nil {
				return CollaborationInvitation{}, err
			}
			u, err := s.repositories.GetUser(ctx, id)
			if err != nil {
				return CollaborationInvitation{}, err
			}
			recipient, recipientID = u.Email, u.ID
		}
		email, err := domain.NormalizeInvitationEmail(recipient)
		if err != nil {
			return CollaborationInvitation{}, err
		}
		invites, err := st.ListCollaborationInvites(ctx, kind, space, "")
		if err != nil {
			return CollaborationInvitation{}, err
		}
		now := time.Now().UTC()
		for _, i := range invites {
			if i.Email == email && i.Status == "pending" && now.Before(i.ExpiresAt) {
				return CollaborationInvitation{}, domain.ErrConflict
			}
		}
		i := domain.CollaborationInvite{ID: domain.NewID("ci_"), Kind: kind, SpaceID: space, Email: email, RecipientID: recipientID, Role: role, Status: "pending", CreatedBy: actor, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}
		if err := st.PutCollaborationInvite(ctx, i); err != nil {
			return CollaborationInvitation{}, err
		}
		if err := s.invitationAudit(ctx, i, actor, "created"); err != nil {
			return CollaborationInvitation{}, err
		}
		if err := s.enqueueInvitationEmail(ctx, i); err != nil {
			return CollaborationInvitation{}, err
		}
		return s.invitationView(ctx, i)
	})
}
func (s *IdentityService) ListCollaborationInvitations(ctx context.Context, actor, kind, space string) ([]CollaborationInvitation, error) {
	if err := s.authorizeInvitation(ctx, actor, kind, space, domain.OrganizationMember); err != nil {
		return nil, err
	}
	st, err := s.invitationStore()
	if err != nil {
		return nil, err
	}
	invites, err := st.ListCollaborationInvites(ctx, kind, space, "")
	if err != nil {
		return nil, err
	}
	out := []CollaborationInvitation{}
	for _, i := range invites {
		v, err := s.invitationView(ctx, i)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *IdentityService) InvitationInbox(ctx context.Context, actor string) ([]CollaborationInvitation, error) {
	u, err := s.repositories.GetUser(ctx, actor)
	if err != nil {
		return nil, err
	}
	email, err := domain.NormalizeInvitationEmail(u.Email)
	if err != nil {
		return []CollaborationInvitation{}, nil
	}
	st, err := s.invitationStore()
	if err != nil {
		return nil, err
	}
	invites, err := st.ListCollaborationInvites(ctx, "", "", email)
	if err != nil {
		return nil, err
	}
	out := []CollaborationInvitation{}
	for _, i := range invites {
		if i.Status != "pending" || !time.Now().Before(i.ExpiresAt) || !invitationRecipient(i, u) {
			continue
		}
		v, err := s.invitationView(ctx, i)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *IdentityService) GetCollaborationInvitation(ctx context.Context, actor, id string) (CollaborationInvitation, error) {
	st, err := s.invitationStore()
	if err != nil {
		return CollaborationInvitation{}, err
	}
	i, err := st.GetCollaborationInvite(ctx, id)
	if err != nil {
		return CollaborationInvitation{}, err
	}
	u, err := s.repositories.GetUser(ctx, actor)
	if err != nil {
		return CollaborationInvitation{}, err
	}
	if !invitationRecipient(i, u) && s.authorizeInvitation(ctx, actor, i.Kind, i.SpaceID, domain.OrganizationMember) != nil {
		return CollaborationInvitation{}, domain.ErrNotFound
	}
	return s.invitationView(ctx, i)
}

// The exclusive identity transaction serializes acceptance with cancellation,
// inviter demotion, recipient offboarding and other acceptors on every server.
func (s *IdentityService) ActOnCollaborationInvitation(ctx context.Context, actor, id, action string) (CollaborationInvitation, error) {
	return identityResult(ctx, s, func(ctx context.Context) (CollaborationInvitation, error) {
		st, err := s.invitationStore()
		if err != nil {
			return CollaborationInvitation{}, err
		}
		i, err := st.GetCollaborationInvite(ctx, id)
		if err != nil {
			return CollaborationInvitation{}, err
		}
		u, err := s.repositories.GetUser(ctx, actor)
		if err != nil {
			return CollaborationInvitation{}, err
		}
		now := time.Now().UTC()
		switch action {
		case "accept":
			if !invitationRecipient(i, u) {
				return CollaborationInvitation{}, domain.ErrForbidden
			}
			current := s.collaborationRole(ctx, actor, i.Kind, i.SpaceID)
			if i.Status == "accepted" && i.AcceptedBy == actor && current.AtLeast(i.Role) {
				return s.invitationView(ctx, i)
			}
			if i.Status != "pending" || !now.Before(i.ExpiresAt) {
				return CollaborationInvitation{}, domain.ErrConflict
			}
			if err := s.authorizeInvitation(ctx, i.CreatedBy, i.Kind, i.SpaceID, i.Role); err != nil {
				return CollaborationInvitation{}, err
			}
			if !current.AtLeast(i.Role) {
				if i.Kind == "organization" {
					err = s.mutateUpdateOrganizationMember(ctx, i.CreatedBy, i.SpaceID, actor, i.Role)
				} else {
					err = s.UpdateEnterpriseMember(ctx, i.CreatedBy, i.SpaceID, actor, domain.EnterpriseRole(i.Role))
				}
				if err != nil {
					return CollaborationInvitation{}, err
				}
			}
			i.Status, i.AcceptedBy = "accepted", actor
		case "decline":
			if !invitationRecipient(i, u) {
				return CollaborationInvitation{}, domain.ErrForbidden
			}
			if i.Status != "pending" {
				return CollaborationInvitation{}, domain.ErrConflict
			}
			i.Status = "revoked"
		case "revoke", "resend":
			if err := s.authorizeInvitation(ctx, actor, i.Kind, i.SpaceID, i.Role); err != nil {
				return CollaborationInvitation{}, err
			}
			if i.Status == "accepted" {
				return CollaborationInvitation{}, domain.ErrConflict
			}
			if i.Status == "revoked" {
				if action == "revoke" {
					return s.invitationView(ctx, i)
				}
				return CollaborationInvitation{}, domain.ErrConflict
			}
			i.Status = "revoked"
		default:
			return CollaborationInvitation{}, domain.ErrValidation
		}
		if action == "resend" {
			// A stale expired invitation must not create a second active invitation.
			all, err := st.ListCollaborationInvites(ctx, i.Kind, i.SpaceID, "")
			if err != nil {
				return CollaborationInvitation{}, err
			}
			for _, other := range all {
				if other.ID != i.ID && other.Email == i.Email && other.Status == "pending" && now.Before(other.ExpiresAt) {
					return CollaborationInvitation{}, domain.ErrConflict
				}
			}
		}
		i.UpdatedAt = now
		if err = st.PutCollaborationInvite(ctx, i); err != nil {
			return CollaborationInvitation{}, err
		}
		if err = s.invitationAudit(ctx, i, actor, action); err != nil {
			return CollaborationInvitation{}, err
		}
		if action == "resend" {
			i.ID, i.Status, i.CreatedBy = domain.NewID("ci_"), "pending", actor
			i.CreatedAt, i.UpdatedAt, i.ExpiresAt = now, now, now.Add(7*24*time.Hour)
			if err = st.PutCollaborationInvite(ctx, i); err != nil {
				return CollaborationInvitation{}, err
			}
			if err = s.invitationAudit(ctx, i, actor, "created"); err != nil {
				return CollaborationInvitation{}, err
			}
			if err := s.enqueueInvitationEmail(ctx, i); err != nil {
				return CollaborationInvitation{}, err
			}
		}
		return s.invitationView(ctx, i)
	})
}
