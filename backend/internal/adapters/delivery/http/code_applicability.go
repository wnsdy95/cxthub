package http

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"net/http"
)

func (s *Server) SetCodeApplicability(q inbound.CodeApplicabilityQuery) { s.codeApplicability = q }
func (s *Server) queryCodeApplicability(w http.ResponseWriter, r *http.Request) {
	if s.codeApplicability == nil {
		s.writeError(w, 503, "unavailable", "Code applicability query unavailable")
		return
	}
	q := r.URL.Query()
	out, err := s.codeApplicability.QueryCodeApplicability(r.Context(), s.repoID(r), domain.CodeSelection{CodeCommit: q.Get("code_commit"), SourceCommit: q.Get("source_commit"), SourceParent: q.Get("source_parent"), Paths: q["path"]})
	s.respond(w, out, err)
}
