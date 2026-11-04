package documentquery

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type visibleReadAdmissionResources struct {
	waiting  chan struct{}
	admit    chan struct{}
	released atomic.Int32
}

func (resources *visibleReadAdmissionResources) AcquireInteractive(ctx context.Context) (context.Context, func(), error) {
	close(resources.waiting)
	select {
	case <-resources.admit:
		return ctx, func() { resources.released.Add(1) }, nil
	case <-ctx.Done():
		return ctx, nil, ctx.Err()
	}
}

func (*visibleReadAdmissionResources) AcquireBulk(context.Context) (context.Context, func(), error) {
	panic("visible read acquired bulk resources before authorization")
}

type visibleReadAuthorizationScope struct {
	Scope
	repo  domain.ContentHash
	calls atomic.Int32
}

func (scope *visibleReadAuthorizationScope) AuthorizeDocumentRead(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	scope.calls.Add(1)
	if repo != scope.repo {
		return ctx, domain.ErrIntegrity
	}
	return inbound.DocumentReadAuthorizer(ctx)(ctx, repo)
}

func assertVisibleReadAuthorizationAfterAdmission(test *testing.T, read func(context.Context, *Queries, domain.DocVisibleSelector) error) {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		return bound, domain.ErrForbidden
	})
	selection := domain.DocVisibleSelector{Version: 1, Purpose: "units", Repo: domain.HashContent([]byte("repo")), Hash: domain.HashContent([]byte("document")),
		Generation: domain.HashContent([]byte("generation")), View: "all", Total: 1, Limit: 1}
	resources := &visibleReadAdmissionResources{waiting: make(chan struct{}), admit: make(chan struct{})}
	scope := &visibleReadAuthorizationScope{repo: selection.Repo}
	queries := &Queries{scope: scope, view: ViewDependencies{Resources: resources}}
	done := make(chan error, 1)
	go func() { done <- read(ctx, queries, selection) }()
	select {
	case <-resources.waiting:
	case <-ctx.Done():
		test.Fatal("query did not wait for interactive admission", ctx.Err())
	}
	if scope.calls.Load() != 0 {
		test.Fatal("query authorized before queued interactive admission")
	}
	close(resources.admit)
	select {
	case err := <-done:
		if !errors.Is(err, domain.ErrForbidden) || scope.calls.Load() != 1 || resources.released.Load() != 1 {
			test.Fatal("query ignored post-admission revocation or leaked admission", err, scope.calls.Load(), resources.released.Load())
		}
	case <-ctx.Done():
		test.Fatal("query did not finish after interactive admission", ctx.Err())
	}
}

func TestDocVisiblePageReauthorizesAfterInteractiveAdmission(test *testing.T) {
	assertVisibleReadAuthorizationAfterAdmission(test, func(ctx context.Context, queries *Queries, selection domain.DocVisibleSelector) error {
		page, err := queries.readDocVisiblePage(ctx, selection, true)
		if !reflect.DeepEqual(page, domain.DocVisiblePage{}) {
			return domain.ErrIntegrity
		}
		return err
	})
}
