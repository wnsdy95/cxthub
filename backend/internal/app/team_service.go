package app

import (
	"context"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *IdentityService) teamForActor(ctx context.Context, actor, org, id string, manage bool) (domain.Team, error) {
	if s.teams == nil || s.organization == nil {
		return domain.Team{}, domain.ErrForbidden
	}
	role, ok := s.OrganizationRoleOf(ctx, org, actor)
	if !ok {
		return domain.Team{}, domain.ErrForbidden
	}
	team, err := s.teams.GetTeam(ctx, id)
	if err != nil {
		return team, err
	}
	if team.OrganizationID != org {
		return domain.Team{}, domain.ErrNotFound
	}
	if !manage || role.AtLeast(domain.OrganizationAdmin) {
		return team, nil
	}
	members, err := s.teams.ListTeamMembers(ctx, id)
	if err != nil {
		return domain.Team{}, err
	}
	for _, m := range members {
		if m.UserID == actor && m.Role == domain.TeamMaintainer {
			return team, nil
		}
	}
	return domain.Team{}, domain.ErrForbidden
}
func (s *IdentityService) CreateTeam(ctx context.Context, actor, org, name, slug, description string) (domain.Team, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.Team, error) {
		if s.teams == nil {
			return domain.Team{}, domain.ErrForbidden
		}
		role, ok := s.OrganizationRoleOf(ctx, org, actor)
		if !ok || !role.AtLeast(domain.OrganizationAdmin) {
			return domain.Team{}, domain.ErrForbidden
		}
		name = strings.TrimSpace(name)
		slug = strings.ToLower(strings.TrimSpace(slug))
		if slug == "" {
			slug = domain.Slugify(name, "team")
		}
		team := domain.Team{ID: domain.NewID("team_"), OrganizationID: org, Name: name, Slug: slug, Description: strings.TrimSpace(description), CreatedAt: time.Now().UTC()}
		if err := domain.ValidateTeam(team); err != nil {
			return domain.Team{}, err
		}
		if err := s.teams.CreateTeam(ctx, team); err != nil {
			return domain.Team{}, err
		}
		err := s.organization.AppendOrganizationAudit(ctx, organizationAudit(org, actor, "team.created", "team", team.ID, "", time.Now().UTC()))
		return team, err
	})
}
func (s *IdentityService) ListTeams(ctx context.Context, actor, org string) ([]domain.Team, error) {
	if s.teams == nil {
		return nil, domain.ErrForbidden
	}
	if _, ok := s.OrganizationRoleOf(ctx, org, actor); !ok {
		return nil, domain.ErrForbidden
	}
	return s.teams.ListTeams(ctx, org)
}
func (s *IdentityService) DeleteTeam(ctx context.Context, actor, org, id string) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		if _, err := s.teamForActor(ctx, actor, org, id, true); err != nil {
			return err
		}
		// Team deletion changes all grants. Only organization administrators can do it.
		if role, _ := s.OrganizationRoleOf(ctx, org, actor); !role.AtLeast(domain.OrganizationAdmin) {
			return domain.ErrForbidden
		}
		if err := s.teams.DeleteTeam(ctx, id); err != nil {
			return err
		}
		return s.organization.AppendOrganizationAudit(ctx, organizationAudit(org, actor, "team.deleted", "team", id, "", time.Now().UTC()))
	})
}
func (s *IdentityService) ListTeamMembers(ctx context.Context, actor, org, id string) ([]domain.TeamMembership, error) {
	if _, err := s.teamForActor(ctx, actor, org, id, false); err != nil {
		return nil, err
	}
	return s.teams.ListTeamMembers(ctx, id)
}
func (s *IdentityService) UpdateTeamMember(ctx context.Context, actor, org, id, target string, role domain.TeamRole) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		if _, err := s.teamForActor(ctx, actor, org, id, true); err != nil {
			return err
		}
		if _, ok := s.OrganizationRoleOf(ctx, org, target); !ok {
			return domain.ErrNotFound
		}
		m := domain.TeamMembership{TeamID: id, OrganizationID: org, UserID: target, Role: role, CreatedAt: time.Now().UTC()}
		if err := domain.ValidateTeamMembership(m); err != nil {
			return err
		}
		if err := s.teams.PutTeamMember(ctx, m); err != nil {
			return err
		}
		return s.organization.AppendOrganizationAudit(ctx, organizationAudit(org, actor, "team.member.updated", "team", id, target+":"+string(role), time.Now().UTC()))
	})
}
func (s *IdentityService) RemoveTeamMember(ctx context.Context, actor, org, id, target string) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		if _, err := s.teamForActor(ctx, actor, org, id, actor != target); err != nil {
			return err
		}
		if err := s.teams.RemoveTeamMember(ctx, id, target); err != nil {
			return err
		}
		return s.organization.AppendOrganizationAudit(ctx, organizationAudit(org, actor, "team.member.removed", "team", id, target, time.Now().UTC()))
	})
}
func (s *IdentityService) ListTeamRepositories(ctx context.Context, actor, org, id string) ([]domain.TeamRepositoryGrant, error) {
	if _, err := s.teamForActor(ctx, actor, org, id, false); err != nil {
		return nil, err
	}
	grants, err := s.teams.ListTeamRepositoryGrants(ctx, id)
	if err != nil {
		return nil, err
	}
	out := []domain.TeamRepositoryGrant{}
	// Team membership listings must not reveal private repository IDs to other teams.
	for _, g := range grants {
		r, err := s.repositories.GetRepository(ctx, g.RepositoryID)
		if err != nil {
			return nil, err
		}
		if _, ok := s.RoleOf(ctx, g.RepositoryID, actor); ok || r.IsPublic() {
			out = append(out, g)
		}
	}
	return out, nil
}
func (s *IdentityService) SetTeamRepository(ctx context.Context, actor, org, id, repository string, role domain.MemberRole) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		if _, err := s.teamForActor(ctx, actor, org, id, true); err != nil {
			return err
		}
		if !s.IsOwner(ctx, repository, actor) {
			return domain.ErrForbidden
		}
		organization, err := s.organization.GetOrganization(ctx, org)
		if err != nil {
			return err
		}
		r, err := s.repositories.GetRepository(ctx, repository)
		if err != nil {
			return err
		}
		if r.OwnerNamespaceID != organization.NamespaceID {
			return domain.ErrForbidden
		}
		grant := domain.TeamRepositoryGrant{TeamID: id, OrganizationID: org, RepositoryID: repository, Role: role, CreatedAt: time.Now().UTC()}
		if err := domain.ValidateTeamRepositoryGrant(grant); err != nil {
			return err
		}
		if err := s.teams.PutTeamRepositoryGrant(ctx, grant); err != nil {
			return err
		}
		return s.organization.AppendOrganizationAudit(ctx, organizationAudit(org, actor, "team.repository.updated", "team", id, repository+":"+string(role), time.Now().UTC()))
	})
}
func (s *IdentityService) RemoveTeamRepository(ctx context.Context, actor, org, id, repository string) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		if _, err := s.teamForActor(ctx, actor, org, id, true); err != nil {
			return err
		}
		if !s.IsOwner(ctx, repository, actor) {
			return domain.ErrForbidden
		}
		if err := s.teams.RemoveTeamRepositoryGrant(ctx, id, repository); err != nil {
			return err
		}
		return s.organization.AppendOrganizationAudit(ctx, organizationAudit(org, actor, "team.repository.removed", "team", id, repository, time.Now().UTC()))
	})
}

type TeamPermissions struct {
	CanManage bool `json:"can_manage"`
	CanDelete bool `json:"can_delete"`
}

func (s *IdentityService) TeamPermissions(ctx context.Context, actor, org, id string) (TeamPermissions, error) {
	if _, err := s.teamForActor(ctx, actor, org, id, false); err != nil {
		return TeamPermissions{}, err
	}
	_, err := s.teamForActor(ctx, actor, org, id, true)
	role, _ := s.OrganizationRoleOf(ctx, org, actor)
	return TeamPermissions{CanManage: err == nil, CanDelete: role.AtLeast(domain.OrganizationAdmin)}, nil
}
