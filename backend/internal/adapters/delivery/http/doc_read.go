package http

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Server) getDocEvents(w http.ResponseWriter, r *http.Request) {
	query, err := docReadQuery(r, "projection", "view", "offset", "limit", "base")
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	if query.Has("projection") {
		s.getDocVisiblePage(w, r, query)
		return
	}
	offset, limit := -1, 50
	view := query.Get("view")
	if view != "" {
		limit = 20
	}
	if v := query.Get("offset"); v != "" {
		offset, err = strconv.Atoi(v)
	}
	if err == nil {
		if v := query.Get("limit"); v != "" {
			limit, err = strconv.Atoi(v)
		}
	}
	if err != nil {
		s.respond(w, nil, fmt.Errorf("%w: invalid event range", domain.ErrValidation))
		return
	}
	base := domain.ContentHash(query.Get("base"))
	if err = domain.ValidateOptionalContentHash(base); err != nil {
		s.respond(w, nil, fmt.Errorf("%w: invalid base", domain.ErrValidation))
		return
	}
	if view != "" {
		viewer := s.documentViews
		if viewer == nil {
			s.writeError(w, 503, "unavailable", "range event paging unavailable")
			return
		}
		request, release, ok := s.documentResponseRequest(w, r)
		if !ok {
			return
		}
		defer release()
		repo := s.repoID(r)
		ctx := inbound.WithDocumentReadAuthorizer(request.Context(), s.documentReadAuthorizer(r, repo))
		out, err := viewer.ReadDocViewEvents(ctx, repo, domain.ContentHash(r.PathValue("hash")), base, offset, limit, view)
		s.respond(w, out, err)
		return
	}
	r, release, admitted := s.admitResourceRequest(w, r, app.ResourceBulk)
	if !admitted {
		return
	}
	defer release()
	reader := s.documentEvents
	if reader == nil {
		s.writeError(w, 503, "unavailable", "event paging unavailable")
		return
	}
	repo := s.repoID(r)
	ctx := inbound.WithDocumentReadAuthorizer(r.Context(), s.documentReadAuthorizer(r, repo))
	out, err := reader.ReadDocEvents(ctx, repo, domain.ContentHash(r.PathValue("hash")), base, offset, limit)
	s.respond(w, out, err)
}

func (s *Server) getDocEventFragment(w http.ResponseWriter, r *http.Request) {
	query, err := docReadQuery(r, "index", "offset", "budget")
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	reader := s.documentViews
	if reader == nil {
		s.writeError(w, 503, "unavailable", "event fragment paging unavailable")
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
	request, release, ok := s.documentResponseRequest(w, r)
	if !ok {
		return
	}
	defer release()
	repo := s.repoID(r)
	ctx := inbound.WithDocumentReadAuthorizer(request.Context(), s.documentReadAuthorizer(r, repo))
	out, err := reader.ReadDocViewFragment(ctx, repo, domain.ContentHash(r.PathValue("hash")), values["index"], values["offset"], values["budget"])
	s.respond(w, out, err)
}

func (server *Server) documentReadAuthorizer(request *http.Request, repo domain.ContentHash) inbound.DocumentReadAuthorization {
	if server.documentQueryProfile == inbound.DocumentQueryVerifiedRoots {
		return server.memoryReadAuthorizer(request, repo)
	}
	return server.docVisibleReadAuthorizer(request, repo)
}

func docReadQuery(r *http.Request, keys ...string) (url.Values, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, domain.ErrValidation
	}
	for _, key := range keys {
		if len(query[key]) > 1 {
			return nil, domain.ErrValidation
		}
	}
	return query, nil
}
