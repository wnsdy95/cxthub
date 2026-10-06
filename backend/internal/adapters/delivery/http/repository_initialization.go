package http

import (
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"io"
	"net/http"
)

func (s *Server) beginRepositoryInitialization(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, 415, "bad_request", "Content-Type must be application/json")
		return
	}
	var in domain.RepositoryInitializationRequest
	if !s.decodeRepositoryInitialization(w, r, &in) {
		return
	}
	user, _ := userFrom(r.Context())
	out, err := s.b.BeginRepositoryInitialization(r.Context(), user.ID, s.repoID(r), in)
	s.respond(w, out, err)
}
func (s *Server) finalizeRepositoryInitialization(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, 415, "bad_request", "Content-Type must be application/json")
		return
	}
	var in domain.RepositoryInitializationFinalize
	if !s.decodeRepositoryInitialization(w, r, &in) {
		return
	}
	out, err := s.b.FinalizeRepositoryInitialization(r.Context(), s.repoID(r), in)
	s.respond(w, out, err)
}

// The two initialization messages deliberately reject unrecognized authority
// fields. Keep this bound local rather than changing legacy endpoint decoding.
func (s *Server) decodeRepositoryInitialization(w http.ResponseWriter, r *http.Request, in any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(in); err != nil {
		s.writeError(w, 400, "bad_request", "invalid initialization request")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		s.writeError(w, 400, "bad_request", "one initialization request required")
		return false
	}
	return true
}

func (s *Server) getRepositoryInitialization(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.GetRepositoryInitialization(r.Context(), s.repoID(r))
	s.respond(w, out, err)
}
