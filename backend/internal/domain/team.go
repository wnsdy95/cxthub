package domain

import "time"

type Team struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	Name           string    `json:"name"`
	Slug           string    `json:"slug"`
	Description    string    `json:"description,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type TeamRole string

const (
	TeamMember     TeamRole = "member"
	TeamMaintainer TeamRole = "maintainer"
)

type TeamMembership struct {
	Source         string    `json:"source,omitempty"`
	TeamID         string    `json:"team_id"`
	OrganizationID string    `json:"organization_id"`
	UserID         string    `json:"user_id"`
	Role           TeamRole  `json:"role"`
	CreatedAt      time.Time `json:"created_at"`
}
type TeamRepositoryGrant struct {
	TeamID         string     `json:"team_id"`
	OrganizationID string     `json:"organization_id"`
	RepositoryID   string     `json:"repository_id"`
	Role           MemberRole `json:"role"`
	CreatedAt      time.Time  `json:"created_at"`
}

// TeamRepositoryAccess is a read projection of current membership and ownership
// facts. The domain decides whether they authorize this repository and actor.
type TeamRepositoryAccess struct {
	TeamID                  string
	RepositoryID            string
	OrganizationNamespaceID string
	UserID                  string
	OrganizationMember      bool
	TeamMember              bool
	Role                    MemberRole
}

func ValidateTeamID(id string) error { return validatePrefixedHexID(id, "team_", 32) }
func ValidateTeam(t Team) error {
	if err := ValidateTeamID(t.ID); err != nil {
		return err
	}
	if err := ValidateOrganizationID(t.OrganizationID); err != nil {
		return err
	}
	if t.Name == "" || len(t.Name) > 100 || !ValidNamespaceSlug(t.Slug) || len(t.Description) > 1000 {
		return ErrValidation
	}
	return nil
}
func ValidateTeamMembership(m TeamMembership) error {
	if err := ValidateTeamID(m.TeamID); err != nil {
		return err
	}
	if err := ValidateOrganizationID(m.OrganizationID); err != nil {
		return err
	}
	if err := ValidateExternalID(m.UserID); err != nil {
		return err
	}
	if m.Role != TeamMember && m.Role != TeamMaintainer {
		return ErrValidation
	}
	return nil
}
func ValidateTeamRepositoryGrant(g TeamRepositoryGrant) error {
	if err := ValidateTeamID(g.TeamID); err != nil {
		return err
	}
	if err := ValidateOrganizationID(g.OrganizationID); err != nil {
		return err
	}
	if err := ValidateRepositoryID(g.RepositoryID); err != nil {
		return err
	}
	if !ValidRole(g.Role) {
		return ErrValidation
	}
	return nil
}

func EffectiveRepositoryRole(repository Repository, members []Membership, actor string, teams []TeamRepositoryAccess, organization OrganizationRepositoryAccess) (MemberRole, bool) {
	role, ok := RepositoryRole(repository, members, actor)
	if actor == "" {
		return "", false
	}
	if organization.IsOwnerOf(repository, actor) {
		return RoleOwner, true
	}
	if base, present := organization.BaseRoleFor(repository, actor); present && (!ok || RoleRank(base) > RoleRank(role)) {
		role, ok = base, true
	}
	for _, grant := range teams {
		if grant.UserID != actor || grant.RepositoryID != repository.ID || !grant.OrganizationMember || !grant.TeamMember || grant.OrganizationNamespaceID == "" || grant.OrganizationNamespaceID != repository.OwnerNamespaceID || !ValidRole(grant.Role) {
			continue
		}
		if !ok || RoleRank(grant.Role) > RoleRank(role) {
			role, ok = grant.Role, true
		}
	}
	return role, ok
}
