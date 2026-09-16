package http

import (
	"context"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/http"
	"strconv"
)

func (s *Server) getDocEvents(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.b.(interface {
		ReadDocEvents(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int, int) (domain.DocEventPage, error)
	})
	if !ok {
		s.writeError(w, 503, "unavailable", "event paging unavailable")
		return
	}
	offset, limit := -1, 50
	var err error
	if v := r.URL.Query().Get("offset"); v != "" {
		offset, err = strconv.Atoi(v)
	}
	if err == nil {
		if v := r.URL.Query().Get("limit"); v != "" {
			limit, err = strconv.Atoi(v)
		}
	}
	if err != nil {
		s.respond(w, nil, fmt.Errorf("%w: invalid event range", domain.ErrValidation))
		return
	}
	base := domain.ContentHash(r.URL.Query().Get("base"))
	if err = domain.ValidateOptionalContentHash(base); err != nil {
		s.respond(w, nil, fmt.Errorf("%w: invalid base", domain.ErrValidation))
		return
	}
	out, err := reader.ReadDocEvents(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("hash")), base, offset, limit)
	s.respond(w, out, err)
}
