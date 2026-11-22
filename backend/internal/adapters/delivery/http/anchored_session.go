package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Server) anchoredSessions(w http.ResponseWriter) (inbound.AnchoredSessions, bool) {
	service, ok := s.b.(inbound.AnchoredSessions)
	if !ok {
		s.writeError(w, http.StatusServiceUnavailable, "anchored_sessions_unavailable", "anchored session service unavailable")
	}
	return service, ok
}

// putAnchoredSession stores the report a member's CLI computed for one
// session. The uploader is the signed-in account, never a body field.
func (s *Server) putAnchoredSession(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, domain.AnchoredSessionBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var body domain.AnchoredSessionBody
	if err := decoder.Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds transport limit")
			return
		}
		s.writeDecodeError(w, err)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		s.writeError(w, http.StatusBadRequest, "bad_request", "expected a single JSON object")
		return
	}
	service, ok := s.anchoredSessions(w)
	if !ok {
		return
	}
	receipt, err := service.PutAnchoredSession(r.Context(), s.repoID(r), domain.ProviderKind(r.PathValue("provider")), r.PathValue("sessionID"), body)
	s.respond(w, receipt, err)
}

func (s *Server) getAnchoredSession(w http.ResponseWriter, r *http.Request) {
	service, ok := s.anchoredSessions(w)
	if !ok {
		return
	}
	session, err := service.GetAnchoredSession(r.Context(), s.repoID(r), domain.ProviderKind(r.PathValue("provider")), r.PathValue("sessionID"))
	s.respond(w, session, err)
}

func (s *Server) listAnchoredSessions(w http.ResponseWriter, r *http.Request) {
	service, ok := s.anchoredSessions(w)
	if !ok {
		return
	}
	sessions, err := service.ListAnchoredSessions(r.Context(), s.repoID(r))
	s.respond(w, sessions, err)
}

func (s *Server) anchoredCommitSegments(w http.ResponseWriter, r *http.Request) {
	service, ok := s.anchoredSessions(w)
	if !ok {
		return
	}
	segments, err := service.AnchoredCommitSegments(r.Context(), s.repoID(r), r.PathValue("sha"))
	s.respond(w, segments, err)
}

// searchAnchored looks for one query in what the sessions tab shows, as far
// as the caller may read it.
func (s *Server) searchAnchored(w http.ResponseWriter, r *http.Request) {
	service, ok := s.b.(inbound.AnchoredSearch)
	if !ok {
		s.writeError(w, http.StatusServiceUnavailable, "anchored_sessions_unavailable", "anchored session service unavailable")
		return
	}
	result, err := service.SearchAnchored(r.Context(), s.repoID(r), r.URL.Query().Get("q"))
	s.respond(w, result, err)
}
