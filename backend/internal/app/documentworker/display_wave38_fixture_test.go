package documentworker

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave38DisplayWorkKey struct{}
type wave38DisplayReadKey struct{}

type wave38DisplayScope struct {
	test                   *testing.T
	workers, works, reads  int
	activeWork, activeRead int
	workError, readError   error
	afterRead              error
}

func (scope *wave38DisplayScope) WorkerContext(ctx context.Context) context.Context {
	scope.workers++
	return ctx
}

func (scope *wave38DisplayScope) WithinWork(ctx context.Context, work func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope.workError != nil {
		return scope.workError
	}
	scope.works++
	scope.activeWork++
	defer func() { scope.activeWork-- }()
	return work(context.WithValue(ctx, wave38DisplayWorkKey{}, true))
}

func (scope *wave38DisplayScope) WithinRepositoryRead(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope.readError != nil {
		return scope.readError
	}
	scope.reads++
	scope.activeRead++
	bound, cancel := context.WithCancel(context.WithValue(ctx, wave38DisplayReadKey{}, true))
	defer cancel()
	defer func() { scope.activeRead-- }()
	return errors.Join(read(bound), scope.afterRead)
}

func (scope *wave38DisplayScope) CanYield(context.Context) bool { return false }

func (scope *wave38DisplayScope) Yield(context.Context, outbound.RootPublicationResources) error {
	scope.test.Fatal("display work introduced a publication yield")
	return domain.ErrIntegrity
}

func (scope *wave38DisplayScope) requireRead(ctx context.Context) {
	scope.test.Helper()
	if scope.activeRead != 1 || ctx.Value(wave38DisplayReadKey{}) != true || ctx.Err() != nil {
		scope.test.Fatal("source escaped its live repository read", scope.activeRead, ctx.Err())
	}
}

func (scope *wave38DisplayScope) requireWrite(ctx context.Context) {
	scope.test.Helper()
	if scope.activeRead != 0 || ctx.Value(wave38DisplayReadKey{}) != nil {
		scope.test.Fatal("durable progress was written inside its source read")
	}
}

func (scope *wave38DisplayScope) AuthorizeDocumentRead(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, ctx.Err()
}

func (scope *wave38DisplayScope) AuthorizeDocumentPreparation(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	scope.test.Fatal("worker source attempted query preparation")
	return ctx, domain.ErrIntegrity
}

func (scope *wave38DisplayScope) WithinDocumentRead(context.Context, domain.ContentHash, func(context.Context) error) error {
	scope.test.Fatal("source opened a second read scope")
	return domain.ErrIntegrity
}

func (scope *wave38DisplayScope) WithinDocumentSnapshot(context.Context, domain.ContentHash, func(context.Context) error) error {
	scope.test.Fatal("source opened a second snapshot scope")
	return domain.ErrIntegrity
}

func (scope *wave38DisplayScope) AcquireInteractive(ctx context.Context) (context.Context, func(), error) {
	scope.requireRead(ctx)
	return ctx, func() {}, nil
}

func (scope *wave38DisplayScope) AcquireBulk(ctx context.Context) (context.Context, func(), error) {
	return scope.AcquireInteractive(ctx)
}
