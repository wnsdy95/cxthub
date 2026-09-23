package http

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type TeamIdentity interface {
	TeamPermissions(context.Context, string, string, string) (app.TeamPermissions, error)
	CreateTeam(context.Context, string, string, string, string, string) (domain.Team, error)
	UpdateTeam(context.Context, string, string, string, app.TeamProfile, app.TeamProfile) (domain.Team, error)
	ListTeams(context.Context, string, string) ([]domain.Team, error)
	DeleteTeam(context.Context, string, string, string) error
	ListTeamMembers(context.Context, string, string, string) ([]domain.TeamMembership, error)
	UpdateTeamMember(context.Context, string, string, string, string, domain.TeamRole) error
	RemoveTeamMember(context.Context, string, string, string, string) error
	ListTeamRepositories(context.Context, string, string, string) ([]domain.TeamRepositoryGrant, error)
	SetTeamRepository(context.Context, string, string, string, string, domain.MemberRole) error
	RemoveTeamRepository(context.Context, string, string, string, string) error
}

func (s *Server) registerTeamRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}/teams", s.requireUser(s.listTeams))
	mux.HandleFunc("POST /api/v1/organizations/{organizationID}/teams", s.requireUser(s.createTeam))
	mux.HandleFunc("PATCH /api/v1/organizations/{organizationID}/teams/{teamID}", s.requireUser(s.updateTeam))
	mux.HandleFunc("DELETE /api/v1/organizations/{organizationID}/teams/{teamID}", s.requireUser(s.deleteTeam))
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}/teams/{teamID}/members", s.requireUser(s.listTeamMembers))
	mux.HandleFunc("PUT /api/v1/organizations/{organizationID}/teams/{teamID}/members/{userID}", s.requireUser(s.putTeamMember))
	mux.HandleFunc("DELETE /api/v1/organizations/{organizationID}/teams/{teamID}/members/{userID}", s.requireUser(s.deleteTeamMember))
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}/teams/{teamID}/repositories", s.requireUser(s.listTeamRepositories))
	mux.HandleFunc("PUT /api/v1/organizations/{organizationID}/teams/{teamID}/repositories/{repositoryID}", s.requireUser(s.putTeamRepository))
	mux.HandleFunc("DELETE /api/v1/organizations/{organizationID}/teams/{teamID}/repositories/{repositoryID}", s.requireUser(s.deleteTeamRepository))
}

type teamView struct {
	domain.Team
	app.TeamPermissions
}

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	org := r.PathValue("organizationID")
	teams, err := s.id.ListTeams(r.Context(), user.ID, org)
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	out := make([]teamView, 0, len(teams))
	for _, team := range teams {
		permissions, err := s.id.TeamPermissions(r.Context(), user.ID, org, team.ID)
		if err != nil {
			s.respond(w, nil, err)
			return
		}
		out = append(out, teamView{Team: team, TeamPermissions: permissions})
	}
	s.respond(w, out, nil)
}

func (s *Server) createTeam(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Slug        string `json:"slug"`
		Description string `json:"description"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := s.id.CreateTeam(r.Context(), u.ID, r.PathValue("organizationID"), body.Name, body.Slug, body.Description)
	s.respond(w, out, err)
}
func (s *Server) deleteTeam(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	err := s.id.DeleteTeam(r.Context(), u.ID, r.PathValue("organizationID"), r.PathValue("teamID"))
	s.respond(w, map[string]string{"status": "deleted"}, err)
}

func (s *Server) updateTeam(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Expected *app.TeamProfile `json:"expected"`
		app.TeamProfile
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.Expected == nil {
		s.respond(w, nil, domain.ErrValidation)
		return
	}
	u, _ := userFrom(r.Context())
	team, err := s.id.UpdateTeam(r.Context(), u.ID, r.PathValue("organizationID"), r.PathValue("teamID"), *body.Expected, body.TeamProfile)
	s.respond(w, team, err)
}
func (s *Server) listTeamMembers(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.ListTeamMembers(r.Context(), u.ID, r.PathValue("organizationID"), r.PathValue("teamID"))
	s.respond(w, out, err)
}
func (s *Server) putTeamMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role domain.TeamRole `json:"role"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	err := s.id.UpdateTeamMember(r.Context(), u.ID, r.PathValue("organizationID"), r.PathValue("teamID"), r.PathValue("userID"), body.Role)
	s.respond(w, map[string]string{"status": "updated"}, err)
}
func (s *Server) deleteTeamMember(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	err := s.id.RemoveTeamMember(r.Context(), u.ID, r.PathValue("organizationID"), r.PathValue("teamID"), r.PathValue("userID"))
	s.respond(w, map[string]string{"status": "removed"}, err)
}
func (s *Server) listTeamRepositories(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.ListTeamRepositories(r.Context(), u.ID, r.PathValue("organizationID"), r.PathValue("teamID"))
	s.respond(w, out, err)
}
func (s *Server) putTeamRepository(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role domain.MemberRole `json:"role"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	err := s.id.SetTeamRepository(r.Context(), u.ID, r.PathValue("organizationID"), r.PathValue("teamID"), r.PathValue("repositoryID"), body.Role)
	s.respond(w, map[string]string{"status": "updated"}, err)
}
func (s *Server) deleteTeamRepository(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	err := s.id.RemoveTeamRepository(r.Context(), u.ID, r.PathValue("organizationID"), r.PathValue("teamID"), r.PathValue("repositoryID"))
	s.respond(w, map[string]string{"status": "removed"}, err)
}

// Caller-specific authorization is computed by the application once. Web uses
// this projection instead of rebuilding team membership rules in TypeScript.
type repositoryAccessView struct {
	domain.Repository
	EffectiveRole        domain.MemberRole `json:"effective_role"`
	CanTransferOwnership bool              `json:"can_transfer_ownership"`
}

func (s *Server) repositoryAccess(ctx context.Context, actor string, repository domain.Repository) repositoryAccessView {
	role, _ := s.id.RoleOf(ctx, repository.ID, actor)
	return repositoryAccessView{Repository: repository, EffectiveRole: role, CanTransferOwnership: s.id.CanTransferOwnership(ctx, repository.ID, actor)}
}
func (s *Server) repositoryAccessList(ctx context.Context, actor string, repositories []domain.Repository) []repositoryAccessView {
	out := make([]repositoryAccessView, 0, len(repositories))
	for _, r := range repositories {
		out = append(out, s.repositoryAccess(ctx, actor, r))
	}
	return out
}
