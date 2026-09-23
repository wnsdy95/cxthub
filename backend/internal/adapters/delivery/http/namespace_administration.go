package http

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type NamespaceAdministration interface {
	RenameCollaborationSpace(context.Context, string, string, string, string, string) (string, error)
	TransferRepositoryNamespace(context.Context, string, string, string, string, string) (domain.Repository, error)
}

func (s *Server) registerNamespaceAdministration(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/organizations/{organizationID}/rename", s.requireUser(s.renameCollaborationSpace))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/rename", s.requireUser(s.renameCollaborationSpace))
	mux.HandleFunc("POST /api/v1/repositories/{repositoryID}/transfer-namespace", s.requireUser(s.transferRepositoryNamespace))
}
func (s *Server) renameCollaborationSpace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Expected string `json:"expected_slug"`
		Slug     string `json:"slug"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	kind, id := invitationScope(r)
	path, err := s.id.RenameCollaborationSpace(r.Context(), u.ID, kind, id, body.Expected, body.Slug)
	s.respond(w, map[string]string{"path": path}, err)
}
func (s *Server) transferRepositoryNamespace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExpectedNamespace string `json:"expected_namespace_id"`
		ExpectedSlug      string `json:"expected_slug"`
		Destination       string `json:"destination"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := s.id.TransferRepositoryNamespace(r.Context(), u.ID, r.PathValue("repositoryID"), body.ExpectedNamespace, body.ExpectedSlug, body.Destination)
	s.respond(w, out, err)
}
