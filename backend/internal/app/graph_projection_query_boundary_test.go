package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type graphQueryBoundaryStore struct {
	*store.FSStore
	outbound.GraphProjectionStore
	outbound.GraphQualificationStore
	outbound.GraphServingStore
	projectionCalls int
}

func (storage *graphQueryBoundaryStore) ReadGraphProjection(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error) {
	storage.projectionCalls++
	return domain.GraphProjectionRead{}, domain.ErrGraphProjectionFailed
}

func (storage *graphQueryBoundaryStore) ReadGraphProjectionStatus(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error) {
	storage.projectionCalls++
	return domain.GraphProjectionRead{}, domain.ErrGraphProjectionFailed
}

func (storage *graphQueryBoundaryStore) ReadQualifiedGraphProjection(ctx context.Context, repo domain.ContentHash) (domain.GraphProjectionRead, error) {
	return storage.ReadGraphProjection(ctx, repo)
}

func (storage *graphQueryBoundaryStore) ReadQualifiedGraphProjectionStatus(ctx context.Context, repo domain.ContentHash) (domain.GraphProjectionRead, error) {
	return storage.ReadGraphProjectionStatus(ctx, repo)
}

func (storage *graphQueryBoundaryStore) RequestGraphProjectionRepair(context.Context, domain.GraphProjectionCorruption) error {
	storage.projectionCalls++
	return domain.ErrGraphProjectionFailed
}

func TestGraphReadAllowlistDoesNotSubstituteCurrentContextOrMemory(test *testing.T) {
	fixture := newEffectiveFixture(test)
	ctx := systemTestContext()
	if err := fixture.st.CompareAndSwapRef(ctx, fixture.repo, domain.Ref{RepoID: fixture.repo,
		Kind: domain.RefBranch, Name: "main", Target: fixture.root}, ""); err != nil {
		test.Fatal(err)
	}
	selection := domain.ContextSelection{Position: "HEAD", Scope: "current"}
	wantContext, err := fixture.svc.QueryContext(ctx, fixture.repo, selection)
	if err != nil {
		test.Fatal(err)
	}
	wantMemory := fixture.query(test, 3)
	storage := &graphQueryBoundaryStore{FSStore: fixture.st}
	fixture.svc.meta = storage
	if err := fixture.svc.ConfigureGraphReads([]domain.ContentHash{fixture.repo}); err != nil {
		test.Fatal(err)
	}
	gotContext, err := fixture.svc.QueryContext(ctx, fixture.repo, selection)
	if err != nil || !reflect.DeepEqual(gotContext, wantContext) {
		test.Fatal("graph-only failure altered current context", err)
	}
	if got := fixture.query(test, 3); !reflect.DeepEqual(got, wantMemory) {
		test.Fatal("graph-only failure altered effective memory")
	}
	updated := fixture.digest
	updated.PreviousMemoryHash = fixture.memory
	updated.Summary = "new memory after completed graph generation"
	if _, err := fixture.svc.PutMemoryDigestCAS(ctx, fixture.repo, updated); err != nil {
		test.Fatal(err)
	}
	if got := fixture.query(test, 3); got.LineageHash == wantMemory.LineageHash {
		test.Fatal("current memory remained pinned to graph generation")
	}
	if storage.projectionCalls != 0 {
		test.Fatal("current AI queries consumed lag-tolerant Web projection", storage.projectionCalls)
	}
}
