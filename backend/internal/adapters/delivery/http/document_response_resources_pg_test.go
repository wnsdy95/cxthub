//go:build postgres

package http

import (
	"context"
	"sync/atomic"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type documentResponsePGStore struct {
	*store.PostgresStore
	calls  atomic.Int64
	active atomic.Int64
}

func (storage *documentResponsePGStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	storage.calls.Add(1)
	storage.active.Add(1)
	defer storage.active.Add(-1)
	return storage.PostgresStore.WithinReadSnapshot(ctx, read)
}

type documentResponsePGQueries struct {
	inbound.DocumentQueries
	retain func()
}

func (queries documentResponsePGQueries) responseContext(ctx context.Context) context.Context {
	return inbound.WithDocumentResponseRetainer(ctx, func(release func()) bool {
		queries.retain()
		return inbound.RetainDocumentResponse(ctx, release)
	})
}

func (queries documentResponsePGQueries) ReadDocViewEvents(ctx context.Context, repo, hash, base domain.ContentHash, offset, limit int, view string) (domain.DocViewPage, error) {
	return queries.DocumentQueries.ReadDocViewEvents(queries.responseContext(ctx), repo, hash, base, offset, limit, view)
}

func (queries documentResponsePGQueries) ReadDocViewFragment(ctx context.Context, repo, hash domain.ContentHash, index, offset, budget int) (domain.DocViewFragment, error) {
	return queries.DocumentQueries.ReadDocViewFragment(queries.responseContext(ctx), repo, hash, index, offset, budget)
}
