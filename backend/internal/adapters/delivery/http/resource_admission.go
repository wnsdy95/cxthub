package http

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/graphdiagnostics"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

const resourceRequestLimit = 2 * time.Minute

// graphBuildRetryAfter is the Retry-After, in seconds, of a graph read that ran,
// loaded its inputs and then found no capacity to finish. The web client retries
// an overloaded read up to four times when the server asks for at most ten
// seconds, and each retry loads the whole repository view again before it can
// fail the same way. A longer delay leaves the retry to the client's slower
// revision recovery. A read that was never admitted did no work and keeps the
// one-second default.
const graphBuildRetryAfter = "15"

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
	w, r, finish := observeGraphRead(w, r)
	defer finish()
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
		queryContext, query := graphdiagnostics.Start(request.Context(), graphdiagnostics.Query)
		if query != nil {
			request = request.WithContext(queryContext)
		}
		value, err := read(request)
		query.End()
		if err != nil {
			return err
		}
		s.respond(w, value, nil)
		return nil
	})
	if err != nil {
		if attempt > 0 && errors.Is(err, domain.ErrResourceOverloaded) {
			w.Header().Set("Retry-After", graphBuildRetryAfter)
		}
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
