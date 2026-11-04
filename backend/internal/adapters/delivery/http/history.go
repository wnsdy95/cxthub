package http

import (
	"context"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/http"
)

func (s *Server) listHistory(w http.ResponseWriter, r *http.Request) {
	r, release, admitted := s.admitResourceRequest(w, r, app.ResourceBulk)
	if !admitted {
		return
	}
	defer release()
	events, err := s.contextBranches.ListHistory(r.Context(), s.repoID(r))
	if events == nil {
		events = []domain.HistoryEvent{}
	}
	s.respond(w, events, err)
}

func (s *Server) recordHistory(w http.ResponseWriter, r *http.Request) {
	s.recordHistoryEvent(w, r, false)
}

// A distinct route prevents an older server from accepting the event while
// silently discarding an unknown memory-selection dependency field.
func (s *Server) recordMemorySelection(w http.ResponseWriter, r *http.Request) {
	s.recordHistoryEvent(w, r, true)
}

func (s *Server) recordHistoryEvent(w http.ResponseWriter, r *http.Request, requireMemorySelection bool) {
	r, release, admitted := s.admitResourceRequest(w, r, app.ResourceBulk)
	if !admitted {
		return
	}
	defer release()
	var e domain.HistoryEvent
	if !s.decodeLimited(w, r, &e, 32<<10) {
		return
	}
	if e.RepoID != string(s.repoID(r)) {
		s.respond(w, nil, fmt.Errorf("%w: history repository does not match route", domain.ErrValidation))
		return
	}
	if requireMemorySelection && e.MemorySelectionParent == "" {
		s.respond(w, nil, fmt.Errorf("%w: memory selection requires its prior position", domain.ErrValidation))
		return
	}
	err := s.historyCommands.RecordHistory(r.Context(), e)
	s.respond(w, map[string]string{"id": e.ID}, err)
}

func (s *Server) promoteRepositoryPR(w http.ResponseWriter, r *http.Request) {
	r, release, admitted := s.admitResourceRequest(w, r, app.ResourceInteractive)
	if !admitted {
		return
	}
	defer release()
	var pr domain.PullRequestMerge
	if !s.decodeLimited(w, r, &pr, 32<<10) {
		return
	}
	out, err := s.prPromoter.DeliverPRPromotion(r.Context(), s.repoID(r), pr)
	s.respond(w, out, err)
}

func (s *Server) enableContextProtocol(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	r, release, admitted := s.admitResourceRequest(w, r, app.ResourceInteractive)
	if !admitted {
		return
	}
	defer release()
	err := s.contextBranches.EnableContextProtocol(r.Context(), s.repoID(r))
	s.respond(w, map[string]int{"context_protocol": 1}, err)
}

// Optional capability keeps lightweight REST test doubles independent of job storage.
type prJobBackend interface {
	SubmitPRPromotion(context.Context, domain.ContentHash, domain.PullRequestMerge) (domain.PRPromotionJob, error)
	ListPRPromotions(context.Context, domain.ContentHash) ([]domain.PRPromotionJob, error)
	RetryPRPromotion(context.Context, domain.ContentHash, string) error
}

func (s *Server) submitPRPromotion(w http.ResponseWriter, r *http.Request) {
	if s.prPromotions == nil {
		s.writeError(w, 503, "unavailable", "PR queue unavailable")
		return
	}
	r, release, admitted := s.admitResourceRequest(w, r, app.ResourceInteractive)
	if !admitted {
		return
	}
	defer release()
	var pr domain.PullRequestMerge
	if !s.decodeLimited(w, r, &pr, 32<<10) {
		return
	}
	j, err := s.prPromotions.SubmitPRPromotion(r.Context(), s.repoID(r), pr)
	s.respond(w, j, err)
}
func (s *Server) listPRPromotions(w http.ResponseWriter, r *http.Request) {
	if s.prPromotions == nil {
		s.writeError(w, 503, "unavailable", "PR queue unavailable")
		return
	}
	r, release, admitted := s.admitResourceRequest(w, r, app.ResourceInteractive)
	if !admitted {
		return
	}
	defer release()
	jobs, err := s.prPromotions.ListPRPromotions(r.Context(), s.repoID(r))
	s.respond(w, jobs, err)
}
func (s *Server) retryPRPromotion(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, 415, "bad_request", "Content-Type must be application/json")
		return
	}
	if s.prPromotions == nil {
		s.writeError(w, 503, "unavailable", "PR queue unavailable")
		return
	}
	r, release, admitted := s.admitResourceRequest(w, r, app.ResourceInteractive)
	if !admitted {
		return
	}
	defer release()
	err := s.prPromotions.RetryPRPromotion(r.Context(), s.repoID(r), r.PathValue("jobID"))
	s.respond(w, map[string]bool{"queued": err == nil}, err)
}
