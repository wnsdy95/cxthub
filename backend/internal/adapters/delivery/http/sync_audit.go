package http

import (
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"net/http"
)

func (s *Server) SetGitSyncAudit(a inbound.GitSyncAudit) { s.gitSyncAudit = a }
func (s *Server) checkGitHubSync(w http.ResponseWriter, r *http.Request) {
	if s.gitSyncAudit == nil {
		s.writeError(w, 503, "unavailable", "GitHub sync audit unavailable")
		return
	}
	var in struct {
		Cursor string `json:"cursor"`
	}
	if !s.decodeLimited(w, r, &in, 1024) {
		return
	}
	out, err := s.gitSyncAudit.CheckGitHubSync(r.Context(), s.repoID(r), in.Cursor)
	s.respond(w, out, err)
}
