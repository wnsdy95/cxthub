package documentworker

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type rangeWave38Scope struct {
	worker   func(context.Context) context.Context
	work     func(context.Context, func(context.Context) error) error
	read     func(context.Context, domain.ContentHash, func(context.Context) error) error
	canYield func(context.Context) bool
	yield    func(context.Context, outbound.RootPublicationResources) error
}

func (scope rangeWave38Scope) WorkerContext(ctx context.Context) context.Context {
	if scope.worker != nil {
		return scope.worker(ctx)
	}
	return ctx
}

func (scope rangeWave38Scope) WithinWork(ctx context.Context, work func(context.Context) error) error {
	if scope.work != nil {
		return scope.work(ctx, work)
	}
	return work(ctx)
}

func (scope rangeWave38Scope) WithinRepositoryRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if scope.read != nil {
		return scope.read(ctx, repo, read)
	}
	return read(ctx)
}

func (scope rangeWave38Scope) CanYield(ctx context.Context) bool {
	return scope.canYield != nil && scope.canYield(ctx)
}

func (scope rangeWave38Scope) Yield(ctx context.Context, resources outbound.RootPublicationResources) error {
	return scope.yield(ctx, resources)
}
