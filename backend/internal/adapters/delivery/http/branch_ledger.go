package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// branchRegistrationBodyLimit bounds a report of branch IDs.
const branchRegistrationBodyLimit = 1 << 20

func (s *Server) branchLedger(w http.ResponseWriter) (inbound.BranchLedger, bool) {
	service, ok := s.b.(inbound.BranchLedger)
	if !ok {
		s.writeError(w, http.StatusServiceUnavailable, "branch_ledger_unavailable", "branch ledger service unavailable")
	}
	return service, ok
}

// putBranchIDs registers the IDs a member's CLI keeps for branches that are
// on the remote. The registering account is the signed-in one.
func (s *Server) putBranchIDs(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, branchRegistrationBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var body struct {
		Branches []domain.BranchRegistration `json:"branches"`
	}
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
	if body.Branches == nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "branches is required")
		return
	}
	service, ok := s.branchLedger(w)
	if !ok {
		return
	}
	results, err := service.RegisterBranchIDs(r.Context(), s.repoID(r), body.Branches)
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	s.respond(w, struct {
		Branches []domain.BranchRegistrationResult `json:"branches"`
	}{Branches: results}, nil)
}

func (s *Server) listBranchLifetimes(w http.ResponseWriter, r *http.Request) {
	service, ok := s.branchLedger(w)
	if !ok {
		return
	}
	lifetimes, err := service.ListBranchLifetimes(r.Context(), s.repoID(r), r.URL.Query().Get("name"))
	s.respond(w, lifetimes, err)
}
