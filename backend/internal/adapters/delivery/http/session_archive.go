package http

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Server) archiveSession(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Archived *bool `json:"archived"`
	}
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		s.writeDecodeError(w, err)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		s.writeError(w, http.StatusBadRequest, "bad_request", "expected a single JSON object")
		return
	}
	if input.Archived == nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "archived must be a boolean")
		return
	}
	service, ok := s.b.(inbound.SessionArchiver)
	if !ok {
		s.writeError(w, http.StatusServiceUnavailable, "archive_unavailable", "session archive service unavailable")
		return
	}
	err := service.SetSessionArchived(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("id")), *input.Archived)
	s.respond(w, struct {
		Archived bool `json:"archived"`
	}{*input.Archived}, err)
}
