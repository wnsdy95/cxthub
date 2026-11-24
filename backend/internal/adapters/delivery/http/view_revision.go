package http

import (
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// getViewRevision answers the mark an open page follows to know when to read
// its lists again. A page asks for it every few seconds, so it is one small
// read. Like every answer of the API it is kept by no cache.
func (s *Server) getViewRevision(w http.ResponseWriter, r *http.Request) {
	service, ok := s.b.(inbound.ViewRevisions)
	if !ok {
		s.writeError(w, http.StatusServiceUnavailable, "view_revision_unavailable", "view revision unavailable")
		return
	}
	revision, err := service.ViewRevision(r.Context(), s.repoID(r))
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	s.respond(w, map[string]string{"revision": revision}, nil)
}
