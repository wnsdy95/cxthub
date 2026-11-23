package documentquery

import (
	"context"
	"sync/atomic"

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
