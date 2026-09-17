package http

import "net/http"

func (s *Server) repositoryView(w http.ResponseWriter, r *http.Request) {
	v, err := s.b.GetRepositoryView(r.Context(), s.repoID(r))
	s.respond(w, v, err)
}
