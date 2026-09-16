package http

import (
	"context"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
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
	promote := s.b.PromoteRepositoryPR
	if durable, ok := s.b.(interface {
		DeliverPRPromotion(context.Context, domain.ContentHash, domain.PullRequestMerge) (inbound.UpdateRefOutput, error)
	}); ok {
		promote = durable.DeliverPRPromotion
	}
	out, err := promote(r.Context(), s.repoID(r), pr)
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

// Optional capability keeps lightweight REST test doubles independent of job storage.
type prJobBackend interface {
	SubmitPRPromotion(context.Context, domain.ContentHash, domain.PullRequestMerge) (domain.PRPromotionJob, error)
	ListPRPromotions(context.Context, domain.ContentHash) ([]domain.PRPromotionJob, error)
	RetryPRPromotion(context.Context, domain.ContentHash, string) error
}

func (s *Server) submitPRPromotion(w http.ResponseWriter, r *http.Request) {
	b, ok := s.b.(prJobBackend)
	if !ok {
		s.writeError(w, 503, "unavailable", "PR queue unavailable")
		return
	}
	var pr domain.PullRequestMerge
	if !s.decodeLimited(w, r, &pr, 32<<10) {
		return
	}
	j, err := b.SubmitPRPromotion(r.Context(), s.repoID(r), pr)
	s.respond(w, j, err)
}
func (s *Server) listPRPromotions(w http.ResponseWriter, r *http.Request) {
	b, ok := s.b.(prJobBackend)
	if !ok {
		s.writeError(w, 503, "unavailable", "PR queue unavailable")
		return
	}
	jobs, err := b.ListPRPromotions(r.Context(), s.repoID(r))
	s.respond(w, jobs, err)
}
func (s *Server) retryPRPromotion(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, 415, "bad_request", "Content-Type must be application/json")
		return
	}
	b, ok := s.b.(prJobBackend)
	if !ok {
		s.writeError(w, 503, "unavailable", "PR queue unavailable")
		return
	}
	err := b.RetryPRPromotion(r.Context(), s.repoID(r), r.PathValue("jobID"))
	s.respond(w, map[string]bool{"queued": err == nil}, err)
}
