package http

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"net/http"
	"strconv"
)

// SetGitChanges is composition-time injection of a separate application service.
func (s *Server) SetGitChanges(changes inbound.GitChanges) { s.gitChanges = changes }
func (s *Server) changesAvailable(w http.ResponseWriter) bool {
	if s.gitChanges == nil {
		s.writeError(w, 503, "unavailable", "Git change verification unavailable")
		return false
	}
	return true
}
func (s *Server) submitGitChange(w http.ResponseWriter, r *http.Request) {
	if !s.changesAvailable(w) {
		return
	}
	var request domain.GitChangeRequest
	if !s.decodeLimited(w, r, &request, 4096) {
		return
	}
	out, err := s.gitChanges.Submit(r.Context(), s.repoID(r), request)
	s.respond(w, out, err)
}
func (s *Server) listGitChanges(w http.ResponseWriter, r *http.Request) {
	if !s.changesAvailable(w) {
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil {
			s.respond(w, nil, domain.ErrValidation)
			return
		}
	}
	out, err := s.gitChanges.List(r.Context(), s.repoID(r), r.URL.Query().Get("cursor"), limit)
	s.respond(w, out, err)
}
func (s *Server) getGitChange(w http.ResponseWriter, r *http.Request) {
	if !s.changesAvailable(w) {
		return
	}
	out, err := s.gitChanges.Get(r.Context(), s.repoID(r), r.PathValue("changeID"))
	s.respond(w, out, err)
}
func (s *Server) retryGitChange(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, 415, "bad_request", "Content-Type must be application/json")
		return
	}
	if !s.changesAvailable(w) {
		return
	}
	err := s.gitChanges.Retry(r.Context(), s.repoID(r), r.PathValue("changeID"))
	s.respond(w, map[string]bool{"queued": err == nil}, err)
}
