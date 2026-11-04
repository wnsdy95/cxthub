package http

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
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

func (s *Server) graphRead(w http.ResponseWriter, r *http.Request, read func(*http.Request) (any, error)) {
	if s.repositoryGraphs == nil {
		s.writeError(w, http.StatusNotImplemented, "graph_query_unavailable", "repository graph query unavailable")
		return
	}
	r, cancel, ok := s.resourceRequestDeadline(w, r)
	if !ok {
		return
	}
	defer cancel()
	attempt := 0
	err := app.WithGraphReadResources(r.Context(), func(bound context.Context) error {
		request := r.WithContext(bound)
		if attempt > 0 {
			request = request.WithContext(inbound.WithRepositoryActor(context.WithValue(bound, userCtxKey, struct{}{}), ""))
			allowed := false
			s.guard(domain.RoleViewer, func(_ http.ResponseWriter, authorized *http.Request) {
				request, allowed = authorized, true
			})(w, request)
			if !allowed {
				return nil
			}
		}
		attempt++
		authorize := s.docVisibleReadAuthorizer(request, s.repoID(request))
		if s.repositoryGraphProfile == inbound.RepositoryGraphPinnedSnapshots {
			authorize = s.memoryReadAuthorizer(request, s.repoID(request))
		}
		request = request.WithContext(inbound.WithDocumentReadAuthorizer(request.Context(), authorize))
		value, err := read(request)
		if err != nil {
			return err
		}
		s.respond(w, value, nil)
		return nil
	})
	if err != nil {
		s.respond(w, nil, err)
	}
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
