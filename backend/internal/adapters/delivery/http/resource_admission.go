package http

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
)

const resourceRequestLimit = 2 * time.Minute

func (s *Server) admitResourceRequest(w http.ResponseWriter, r *http.Request, class app.ResourceClass) (*http.Request, func(), bool) {
	ctx, cancel := context.WithTimeout(r.Context(), resourceRequestLimit)
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(w)
	for _, set := range []func(time.Time) error{controller.SetReadDeadline, controller.SetWriteDeadline} {
		if err := set(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			cancel()
			s.respond(w, nil, err)
			return r, nil, false
		}
	}
	ctx, release, err := app.AcquireResources(ctx, class)
	if err != nil {
		cancel()
		s.respond(w, nil, err)
		return r, nil, false
	}
	return r.WithContext(ctx), func() { cancel(); release() }, true
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
