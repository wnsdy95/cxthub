package http

import (
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/http"
)

func (s *Server) listHistory(w http.ResponseWriter, r *http.Request) {
	events, err := s.b.ListHistory(r.Context(), s.repoID(r))
	if events == nil {
		events = []domain.HistoryEvent{}
	}
	s.respond(w, events, err)
}

func (s *Server) recordHistory(w http.ResponseWriter, r *http.Request) {
	var e domain.HistoryEvent
	if !s.decodeLimited(w, r, &e, 32<<10) {
		return
	}
	if e.RepoID != string(s.repoID(r)) {
		s.respond(w, nil, fmt.Errorf("%w: history repository does not match route", domain.ErrValidation))
		return
	}
	err := s.b.RecordHistory(r.Context(), e)
	s.respond(w, map[string]string{"id": e.ID}, err)
}

func (s *Server) promoteRepositoryPR(w http.ResponseWriter, r *http.Request) {
	var pr domain.PullRequestMerge
	if !s.decodeLimited(w, r, &pr, 32<<10) {
		return
	}
	out, err := s.b.PromoteRepositoryPR(r.Context(), s.repoID(r), pr)
	s.respond(w, out, err)
}

func (s *Server) enableContextProtocol(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	err := s.b.EnableContextProtocol(r.Context(), s.repoID(r))
	s.respond(w, map[string]int{"context_protocol": 1}, err)
}
