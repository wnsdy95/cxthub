package http

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type EnterpriseIdentity interface {
	CreateEnterprise(context.Context, domain.User, string, string) (domain.Enterprise, error)
	ListEnterprises(context.Context, string) ([]domain.Enterprise, error)
	GetEnterprise(context.Context, string, string) (domain.Enterprise, error)
	UpdateEnterprise(context.Context, string, string, app.EnterprisePatch) (domain.Enterprise, error)
	EnterpriseRoleOf(context.Context, string, string) (domain.EnterpriseRole, bool)
	ListEnterpriseMembers(context.Context, string, string) ([]domain.EnterpriseMembership, error)
	UpdateEnterpriseMember(context.Context, string, string, string, domain.EnterpriseRole) error
	RemoveEnterpriseMember(context.Context, string, string, string) error
	ListEnterpriseOrganizations(context.Context, string, string) ([]domain.Organization, error)
	LinkEnterpriseOrganization(context.Context, string, string, string, bool) error
	ListEnterpriseAudit(context.Context, string, string) ([]domain.EnterpriseAuditEvent, error)
	EffectiveOrganizationPolicy(context.Context, string, string) (domain.OrganizationPolicy, error)
}

func (s *Server) registerEnterpriseRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/enterprises", s.requireUser(s.listEnterprises))
	mux.HandleFunc("POST /api/v1/enterprises", s.requireUser(s.createEnterprise))
	mux.HandleFunc("GET /api/v1/enterprises/{enterpriseID}", s.requireUser(s.getEnterprise))
	mux.HandleFunc("PATCH /api/v1/enterprises/{enterpriseID}", s.requireUser(s.patchEnterprise))
	mux.HandleFunc("GET /api/v1/enterprises/{enterpriseID}/members", s.requireUser(s.listEnterpriseMembers))
	mux.HandleFunc("PUT /api/v1/enterprises/{enterpriseID}/members/{userID}", s.requireUser(s.putEnterpriseMember))
	mux.HandleFunc("DELETE /api/v1/enterprises/{enterpriseID}/members/{userID}", s.requireUser(s.deleteEnterpriseMember))
	mux.HandleFunc("GET /api/v1/enterprises/{enterpriseID}/organizations", s.requireUser(s.listEnterpriseOrganizations))
	mux.HandleFunc("PUT /api/v1/enterprises/{enterpriseID}/organizations/{organizationID}", s.requireUser(s.putEnterpriseOrganization))
	mux.HandleFunc("DELETE /api/v1/enterprises/{enterpriseID}/organizations/{organizationID}", s.requireUser(s.deleteEnterpriseOrganization))
	mux.HandleFunc("GET /api/v1/enterprises/{enterpriseID}/audit", s.requireUser(s.listEnterpriseAudit))
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}/effective-policy", s.requireUser(s.effectiveOrganizationPolicy))
}
func (s *Server) listEnterprises(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.ListEnterprises(r.Context(), u.ID)
	s.respond(w, out, err)
}
func (s *Server) createEnterprise(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := s.id.CreateEnterprise(r.Context(), u, body.Name, body.Slug)
	s.respond(w, out, err)
}
func (s *Server) getEnterprise(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.GetEnterprise(r.Context(), u.ID, r.PathValue("enterpriseID"))
	s.respond(w, out, err)
}
func (s *Server) patchEnterprise(w http.ResponseWriter, r *http.Request) {
	var body app.EnterprisePatch
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := s.id.UpdateEnterprise(r.Context(), u.ID, r.PathValue("enterpriseID"), body)
	s.respond(w, out, err)
}
func (s *Server) listEnterpriseMembers(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.ListEnterpriseMembers(r.Context(), u.ID, r.PathValue("enterpriseID"))
	s.respond(w, out, err)
}
func (s *Server) putEnterpriseMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role domain.EnterpriseRole `json:"role"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	err := s.id.UpdateEnterpriseMember(r.Context(), u.ID, r.PathValue("enterpriseID"), r.PathValue("userID"), body.Role)
	s.respond(w, map[string]string{"status": "updated"}, err)
}
func (s *Server) deleteEnterpriseMember(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	err := s.id.RemoveEnterpriseMember(r.Context(), u.ID, r.PathValue("enterpriseID"), r.PathValue("userID"))
	s.respond(w, map[string]string{"status": "removed"}, err)
}
func (s *Server) listEnterpriseOrganizations(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.ListEnterpriseOrganizations(r.Context(), u.ID, r.PathValue("enterpriseID"))
	s.respond(w, out, err)
}
func (s *Server) putEnterpriseOrganization(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.respond(w, nil, domain.ErrValidation)
		return
	}
	u, _ := userFrom(r.Context())
	err := s.id.LinkEnterpriseOrganization(r.Context(), u.ID, r.PathValue("enterpriseID"), r.PathValue("organizationID"), true)
	s.respond(w, map[string]string{"status": "linked"}, err)
}
func (s *Server) deleteEnterpriseOrganization(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	err := s.id.LinkEnterpriseOrganization(r.Context(), u.ID, r.PathValue("enterpriseID"), r.PathValue("organizationID"), false)
	s.respond(w, map[string]string{"status": "removed"}, err)
}
func (s *Server) listEnterpriseAudit(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.ListEnterpriseAudit(r.Context(), u.ID, r.PathValue("enterpriseID"))
	s.respond(w, out, err)
}
func (s *Server) effectiveOrganizationPolicy(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.EffectiveOrganizationPolicy(r.Context(), u.ID, r.PathValue("organizationID"))
	s.respond(w, out, err)
}
