package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type TeamStore interface {
	CreateTeam(context.Context, domain.Team) error
	GetTeam(context.Context, string) (domain.Team, error)
	ListTeams(context.Context, string) ([]domain.Team, error)
	DeleteTeam(context.Context, string) error
	PutTeamMember(context.Context, domain.TeamMembership) error
	RemoveTeamMember(context.Context, string, string) error
	ListTeamMembers(context.Context, string) ([]domain.TeamMembership, error)
	PutTeamRepositoryGrant(context.Context, domain.TeamRepositoryGrant) error
	RemoveTeamRepositoryGrant(context.Context, string, string) error
	ListTeamRepositoryGrants(context.Context, string) ([]domain.TeamRepositoryGrant, error)
	RepositoryTeamAccess(context.Context, string, string) ([]domain.TeamRepositoryAccess, error)
}

// IdentityTransactions serialize changes to the access graph. Context writes
// hold the shared side of this lock until their transaction commits.
type IdentityTransactions interface {
	WithinIdentity(context.Context, func(context.Context) error) error
}
