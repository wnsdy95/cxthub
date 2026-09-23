package http

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type CollaborationInvitations interface {
	CreateCollaborationInvitation(context.Context, string, string, string, string, domain.OrganizationRole, int) (app.CollaborationInvitation, error)
	ListCollaborationInvitations(context.Context, string, string, string) ([]app.CollaborationInvitation, error)
	InvitationInbox(context.Context, string) ([]app.CollaborationInvitation, error)
	GetCollaborationInvitation(context.Context, string, string) (app.CollaborationInvitation, error)
	ActOnCollaborationInvitation(context.Context, string, string, string) (app.CollaborationInvitation, error)
}

func (s *Server) registerCollaborationInvitations(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/me/invitations", s.requireUser(s.invitationInbox))
	mux.HandleFunc("GET /api/v1/invitations/{invitationID}", s.requireUser(s.getCollaborationInvitation))
	mux.HandleFunc("POST /api/v1/invitations/{invitationID}/{action}", s.requireUser(s.actOnCollaborationInvitation))
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}/invitations", s.requireUser(s.listCollaborationInvitations))
	mux.HandleFunc("POST /api/v1/organizations/{organizationID}/invitations", s.requireUser(s.createCollaborationInvitation))
	mux.HandleFunc("GET /api/v1/enterprises/{enterpriseID}/invitations", s.requireUser(s.listCollaborationInvitations))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/invitations", s.requireUser(s.createCollaborationInvitation))
}
func invitationScope(r *http.Request) (string, string) {
	if id := r.PathValue("organizationID"); id != "" {
		return "organization", id
	}
	return "enterprise", r.PathValue("enterpriseID")
}
func (s *Server) invitationInbox(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.InvitationInbox(r.Context(), u.ID)
	s.respond(w, out, err)
}
func (s *Server) getCollaborationInvitation(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.GetCollaborationInvitation(r.Context(), u.ID, r.PathValue("invitationID"))
	s.respond(w, out, err)
}
func (s *Server) listCollaborationInvitations(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	kind, id := invitationScope(r)
	out, err := s.id.ListCollaborationInvitations(r.Context(), u.ID, kind, id)
	s.respond(w, out, err)
}
func (s *Server) createCollaborationInvitation(w http.ResponseWriter, r *http.Request) {
	body := struct {
		Recipient string                  `json:"recipient"`
		Role      domain.OrganizationRole `json:"role"`
		Days      int                     `json:"days"`
	}{Days: 7, Role: domain.OrganizationMember}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	kind, id := invitationScope(r)
	out, err := s.id.CreateCollaborationInvitation(r.Context(), u.ID, kind, id, body.Recipient, body.Role, body.Days)
	s.respond(w, out, err)
}
func (s *Server) actOnCollaborationInvitation(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := s.id.ActOnCollaborationInvitation(r.Context(), u.ID, r.PathValue("invitationID"), r.PathValue("action"))
	s.respond(w, out, err)
}
