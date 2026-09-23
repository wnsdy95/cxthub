package http

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type MCPApplications interface {
	ListMCPApplications(context.Context, string) ([]app.MCPApplication, error)
	RevokeMCPApplication(context.Context, string, string) error
	AccountAudit(context.Context, string) ([]domain.AccountAuditEvent, error)
}

func (s *Server) registerMCPApplications(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/me/mcp-applications", s.requireUser(s.listMCPApplications))
	mux.HandleFunc("DELETE /api/v1/me/mcp-applications/{clientID}", s.requireUser(s.revokeMCPApplication))
	mux.HandleFunc("GET /api/v1/me/audit", s.requireUser(s.accountAudit))
}
func (s *Server) listMCPApplications(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.ListMCPApplications(r.Context(), u.ID)
	s.respond(w, out, err)
}
func (s *Server) revokeMCPApplication(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	err := s.id.RevokeMCPApplication(r.Context(), u.ID, r.PathValue("clientID"))
	s.respond(w, map[string]string{"status": "revoked"}, err)
}
func (s *Server) accountAudit(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.AccountAudit(r.Context(), u.ID)
	s.respond(w, out, err)
}
