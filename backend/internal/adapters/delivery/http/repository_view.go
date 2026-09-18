package http

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/http"
)

func (s *Server) repositoryView(w http.ResponseWriter, r *http.Request) {
	v, err := s.b.GetRepositoryView(r.Context(), s.repoID(r))
	s.respond(w, v, err)
}

func (s *Server) contextQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v, err := s.b.QueryContext(r.Context(), s.repoID(r), domain.ContextSelection{Branch: q.Get("branch"), Position: q.Get("position"), Scope: q.Get("scope")})
	s.respond(w, v, err)
}
