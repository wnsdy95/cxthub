package http

import (
	"encoding/json"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Server) submitDocFinalization(w http.ResponseWriter, r *http.Request) {
	if s.docFinalization == nil {
		s.writeError(w, http.StatusServiceUnavailable, "unavailable", "document finalization unavailable")
		return
	}
	var body inbound.ChunkedDoc
	if !s.decodeLimited(w, r, &body, domain.MaxDocJobManifestBytes+1024) {
		return
	}
	out, err := s.docFinalization.SubmitDocFinalization(r.Context(), s.repoID(r), body)
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(out)
}
func (s *Server) getDocFinalization(w http.ResponseWriter, r *http.Request) {
	if s.docFinalization == nil {
		s.writeError(w, http.StatusServiceUnavailable, "unavailable", "document finalization unavailable")
		return
	}
	out, err := s.docFinalization.GetDocFinalization(r.Context(), s.repoID(r), r.PathValue("jobID"))
	s.respond(w, out, err)
}
