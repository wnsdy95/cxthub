package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

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

func (s *Server) anchoredSummaries(w http.ResponseWriter) (inbound.AnchoredSummaries, bool) {
	service, ok := s.b.(inbound.AnchoredSummaries)
	if !ok {
		s.writeError(w, http.StatusServiceUnavailable, "anchored_sessions_unavailable", "anchored session service unavailable")
	}
	return service, ok
}

// putAnchoredSummary stores the summary a member's CLI made of one of that
// member's sessions at a commit.
func (s *Server) putAnchoredSummary(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, domain.AnchoredSummaryBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var body domain.AnchoredSummaryBody
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
	service, ok := s.anchoredSummaries(w)
	if !ok {
		return
	}
	receipt, err := service.PutAnchoredSummary(r.Context(), s.repoID(r), domain.ProviderKind(r.PathValue("provider")), r.PathValue("sessionID"), r.PathValue("sha"), body)
	s.respond(w, receipt, err)
}

// listAnchoredCommitSummaries lists the summaries at one commit that the
// caller may read.
func (s *Server) listAnchoredCommitSummaries(w http.ResponseWriter, r *http.Request) {
	service, ok := s.anchoredSummaries(w)
	if !ok {
		return
	}
	summaries, err := service.ListAnchoredSummaries(r.Context(), s.repoID(r), []string{r.PathValue("sha")})
	s.respond(w, summaries, err)
}

// readAnchoredMemory lists the summaries along a branch that the caller may
// read, the summary of the newest commit first.
func (s *Server) readAnchoredMemory(w http.ResponseWriter, r *http.Request) {
	service, ok := s.anchoredSummaries(w)
	if !ok {
		return
	}
	limit := 5
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			s.respond(w, nil, fmt.Errorf("%w: limit is a number", domain.ErrValidation))
			return
		}
		limit = parsed
	}
	memory, err := service.ReadAnchoredMemory(r.Context(), s.repoID(r), r.URL.Query().Get("branch"), r.URL.Query()["local"], limit)
	s.respond(w, memory, err)
}

// setAnchoredSessionArchive archives a session or restores it. The body says
// which: {"archived": true} or {"archived": false}.
func (s *Server) setAnchoredSessionArchive(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request struct {
		Archived *bool `json:"archived"`
	}
	if err := decoder.Decode(&request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds transport limit")
			return
		}
		s.writeDecodeError(w, err)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF || request.Archived == nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "expected a single JSON object that says whether the session is archived")
		return
	}
	service, ok := s.anchoredSessions(w)
	if !ok {
		return
	}
	receipt, err := service.SetAnchoredSessionArchived(r.Context(), s.repoID(r), domain.ProviderKind(r.PathValue("provider")), r.PathValue("sessionID"), *request.Archived)
	s.respond(w, receipt, err)
}

// removeAnchoredSession removes a session, and the sessions it spawned, from
// the server and answers what was removed. A removal is remembered: no report
// is taken for a removed session until it is allowed again.
func (s *Server) removeAnchoredSession(w http.ResponseWriter, r *http.Request) {
	service, ok := s.anchoredSessions(w)
	if !ok {
		return
	}
	receipt, err := service.RemoveAnchoredSession(r.Context(), s.repoID(r), domain.ProviderKind(r.PathValue("provider")), r.PathValue("sessionID"))
	s.respond(w, receipt, err)
}

// allowAnchoredSession takes back the removal of a session. Nothing comes
// back by itself: the next upload of a CLI that still holds the tool's
// records stores the session again.
func (s *Server) allowAnchoredSession(w http.ResponseWriter, r *http.Request) {
	service, ok := s.anchoredSessions(w)
	if !ok {
		return
	}
	if err := service.AllowAnchoredSession(r.Context(), s.repoID(r), domain.ProviderKind(r.PathValue("provider")), r.PathValue("sessionID")); err != nil {
		s.respond(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listAnchoredSessionRemovals lists the caller's own removed sessions.
func (s *Server) listAnchoredSessionRemovals(w http.ResponseWriter, r *http.Request) {
	service, ok := s.anchoredSessions(w)
	if !ok {
		return
	}
	removals, err := service.ListAnchoredSessionRemovals(r.Context(), s.repoID(r))
	s.respond(w, removals, err)
}

const (
	// anchoredContentEvents is the most events one page of a conversation's
	// content holds, and the default.
	anchoredContentEvents = 50
	// anchoredContentBudget is the size of a page in bytes of event JSON: the
	// default, and the most a request may ask for. It is the most the
	// document read underneath gives at once.
	anchoredContentBudget = 64 << 10
)

// readAnchoredSessionContent reads a session's conversation as the events
// themselves: whole events where they fit the byte budget, and a part of one
// event where it alone is larger. It answers only what the caller may read,
// and where the event asked for is not open, reading goes on at the next open
// part. It is what a CLI continues a conversation from; the page a browser
// shows is read with the events route.
func (s *Server) readAnchoredSessionContent(w http.ResponseWriter, r *http.Request) {
	service, ok := s.anchoredSessions(w)
	if !ok {
		return
	}
	query := r.URL.Query()
	number := func(name string, fallback int) (int, bool) {
		raw := query.Get(name)
		if raw == "" {
			return fallback, true
		}
		parsed, err := strconv.Atoi(raw)
		return parsed, err == nil
	}
	index, okIndex := number("index", 0)
	offset, okOffset := number("offset", 0)
	limit, okLimit := number("limit", anchoredContentEvents)
	budget, okBudget := number("budget", anchoredContentBudget)
	if !okIndex || !okOffset || !okLimit || !okBudget || budget > anchoredContentBudget {
		s.respond(w, nil, fmt.Errorf("%w: invalid event range", domain.ErrValidation))
		return
	}
	page, err := service.ReadAnchoredSessionFragments(r.Context(), s.repoID(r), domain.ProviderKind(r.PathValue("provider")), r.PathValue("sessionID"), index, offset, limit, budget)
	s.respond(w, page, err)
}
