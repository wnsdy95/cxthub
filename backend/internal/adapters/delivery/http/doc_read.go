package http

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *Server) getDocEvents(w http.ResponseWriter, r *http.Request) {
	query, err := docReadQuery(r, "view", "offset", "limit", "base")
	if err != nil {
		s.respond(w, nil, err)
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
		viewer, ok := s.b.(interface {
			ReadDocViewEvents(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int, int, string) (domain.DocViewPage, error)
		})
		if !ok {
			s.writeError(w, 503, "unavailable", "range event paging unavailable")
			return
		}
		out, err := viewer.ReadDocViewEvents(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("hash")), base, offset, limit, view)
		s.respond(w, out, err)
		return
	}
	r, release, admitted := s.admitResourceRequest(w, r, app.ResourceBulk)
	if !admitted {
		return
	}
	defer release()
	reader, ok := s.b.(interface {
		ReadDocEvents(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int, int) (domain.DocEventPage, error)
	})
	if !ok {
		s.writeError(w, 503, "unavailable", "event paging unavailable")
		return
	}
	out, err := reader.ReadDocEvents(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("hash")), base, offset, limit)
	s.respond(w, out, err)
}

func (s *Server) getDocEventFragment(w http.ResponseWriter, r *http.Request) {
	query, err := docReadQuery(r, "index", "offset", "budget")
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	reader, ok := s.b.(interface {
		ReadDocViewFragment(context.Context, domain.ContentHash, domain.ContentHash, int, int, int) (domain.DocViewFragment, error)
	})
	if !ok {
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
	out, err := reader.ReadDocViewFragment(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("hash")), values["index"], values["offset"], values["budget"])
	s.respond(w, out, err)
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
