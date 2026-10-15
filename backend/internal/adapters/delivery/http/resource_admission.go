package http

import (
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/app"
)

func (s *Server) admitResourceRequest(w http.ResponseWriter, r *http.Request, class app.ResourceClass) (*http.Request, func(), bool) {
	ctx, release, err := app.AcquireResources(r.Context(), class)
	if err != nil {
		s.respond(w, nil, err)
		return r, nil, false
	}
	return r.WithContext(ctx), release, true
}

func (s *Server) bulkRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r, release, ok := s.admitResourceRequest(w, r, app.ResourceBulk)
		if !ok {
			return
		}
		defer release()
		next(w, r)
	}
}
