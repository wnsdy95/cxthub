package http

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// anchoredEventPageLimit is the most events one page of a session's
// conversation holds.
const anchoredEventPageLimit = 20

// anchoredContentAccess resolves what the caller may read of the session's
// conversation. It answers false after writing the error.
func (s *Server) anchoredContentAccess(w http.ResponseWriter, r *http.Request) (domain.AnchoredContentAccess, bool) {
	service, ok := s.anchoredSessions(w)
	if !ok {
		return domain.AnchoredContentAccess{}, false
	}
	access, err := service.AnchoredSessionContentAccess(r.Context(), s.repoID(r), domain.ProviderKind(r.PathValue("provider")), r.PathValue("sessionID"))
	if err != nil {
		s.respond(w, nil, err)
		return domain.AnchoredContentAccess{}, false
	}
	return access, true
}

// getAnchoredSessionEvents reads a session's conversation by the session, not
// by the hash of the document that holds it: the uploader reads all of it,
// another member only the events that are open. A page never crosses into
// what the caller may not read.
func (s *Server) getAnchoredSessionEvents(w http.ResponseWriter, r *http.Request) {
	query, err := docReadQuery(r, "offset", "limit", "view")
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	offset, err := strconv.Atoi(query.Get("offset"))
	limit, view := anchoredEventPageLimit, "all"
	if raw := query.Get("limit"); err == nil && raw != "" {
		limit, err = strconv.Atoi(raw)
	}
	switch query.Get("view") {
	case "", "all":
	case "raw":
		// One event in full, for a body the page deferred.
		view, limit = "raw", 1
	default:
		err = domain.ErrValidation
	}
	if err != nil || offset < 0 || limit < 1 || limit > anchoredEventPageLimit {
		s.respond(w, nil, fmt.Errorf("%w: invalid event range", domain.ErrValidation))
		return
	}
	viewer := s.documentViews
	if viewer == nil {
		s.writeError(w, http.StatusServiceUnavailable, "unavailable", "range event paging unavailable")
		return
	}
	access, ok := s.anchoredContentAccess(w, r)
	if !ok {
		return
	}
	span, open := access.Span(offset)
	doc, first, end, held := access.Part(offset)
	if !open || !held {
		s.respond(w, nil, domain.ErrNotFound)
		return
	}
	// A page ends where the open span ends, and where the document that
	// holds its events ends: the next page goes on in the next document.
	if end > span[1] {
		end = span[1]
	}
	if limit > end-offset {
		limit = end - offset
	}
	request, release, ok := s.documentResponseRequest(w, r)
	if !ok {
		return
	}
	defer release()
	repo := s.repoID(r)
	ctx := inbound.WithDocumentReadAuthorizer(request.Context(), s.documentReadAuthorizer(r, repo))
	page, err := viewer.ReadDocViewEvents(ctx, repo, doc, offset-first, limit, view)
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	out := domain.AnchoredEventPage{Events: []domain.DocViewEvent{}, Offset: offset, Next: -1}
	// What the reader returned is held to what was asked: in order from the
	// offset, at most the page, and never past the end of the open span or
	// of the document. The reader counts events within its document; the
	// page counts them within the conversation.
	for _, event := range page.Events {
		event.Index += first
		if len(out.Events) < limit && event.Index == offset+len(out.Events) && event.Index < end {
			out.Events = append(out.Events, event)
		}
	}
	if next := offset + len(out.Events); len(out.Events) > 0 && next < span[1] {
		out.Next = next
	}
	s.respond(w, out, nil)
}

// getAnchoredSessionEventFragment reads a piece of one large event of a
// session's conversation, under the same rule as its pages.
func (s *Server) getAnchoredSessionEventFragment(w http.ResponseWriter, r *http.Request) {
	query, err := docReadQuery(r, "index", "offset", "budget")
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	values := map[string]int{"index": -1, "offset": 0, "budget": 64 << 10}
	for key := range values {
		if raw := query.Get(key); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				s.respond(w, nil, domain.ErrValidation)
				return
			}
			values[key] = value
		}
	}
	viewer := s.documentViews
	if viewer == nil {
		s.writeError(w, http.StatusServiceUnavailable, "unavailable", "event fragment paging unavailable")
		return
	}
	access, ok := s.anchoredContentAccess(w, r)
	if !ok {
		return
	}
	_, open := access.Span(values["index"])
	doc, first, _, held := access.Part(values["index"])
	if !open || !held {
		s.respond(w, nil, domain.ErrNotFound)
		return
	}
	request, release, ok := s.documentResponseRequest(w, r)
	if !ok {
		return
	}
	defer release()
	repo := s.repoID(r)
	ctx := inbound.WithDocumentReadAuthorizer(request.Context(), s.documentReadAuthorizer(r, repo))
	out, err := viewer.ReadDocViewFragment(ctx, repo, doc, values["index"]-first, values["offset"], values["budget"])
	// The reader counts events within its document; the answer counts them
	// within the conversation.
	out.Index += first
	s.respond(w, out, err)
}
