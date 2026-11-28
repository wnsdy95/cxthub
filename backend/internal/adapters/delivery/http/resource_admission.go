package http

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

const resourceRequestLimit = 2 * time.Minute

type documentResponseResources struct {
	mutex    sync.Mutex
	open     bool
	releases []func()
}

func (scope *documentResponseResources) retain(release func()) bool {
	scope.mutex.Lock()
	defer scope.mutex.Unlock()
	if !scope.open {
		return false
	}
	scope.releases = append(scope.releases, release)
	return true
}

func (scope *documentResponseResources) close() {
	scope.mutex.Lock()
	scope.open = false
	releases := scope.releases
	scope.releases = nil
	scope.mutex.Unlock()
	for _, release := range releases {
		release()
	}
}

func (s *Server) documentResponseRequest(w http.ResponseWriter, r *http.Request) (*http.Request, func(), bool) {
	request, cancel, ok := s.resourceRequestDeadline(w, r)
	if !ok {
		return request, nil, false
	}
	owner := &documentResponseResources{open: true}
	ctx := inbound.WithDocumentResponseRetainer(request.Context(), owner.retain)
	return request.WithContext(ctx), func() { cancel(); owner.close() }, true
}

func (s *Server) admitResourceRequest(w http.ResponseWriter, r *http.Request, class app.ResourceClass) (*http.Request, func(), bool) {
	r, cancel, ok := s.resourceRequestDeadline(w, r)
	if !ok {
		return r, nil, false
	}
	ctx, release, err := app.AcquireResources(r.Context(), class)
	if err != nil {
		cancel()
		s.respond(w, nil, err)
		return r, nil, false
	}
	return r.WithContext(ctx), func() { cancel(); release() }, true
}

func (s *Server) resourceRequestDeadline(w http.ResponseWriter, r *http.Request) (*http.Request, func(), bool) {
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
	return r.WithContext(ctx), cancel, true
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
