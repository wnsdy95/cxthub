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
