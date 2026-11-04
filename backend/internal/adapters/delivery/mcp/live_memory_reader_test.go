package mcp

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type liveMemoryFixtureReader struct {
	inbound.MemoryReadScope
	inbound.EffectiveMemoryQuery
	inbound.BranchMemoryQuery
	inbound.MemoryPositionQuery
}

type branchMemoryQueryFunc func(context.Context, domain.ContentHash, string, domain.ContentHash, string) (domain.MemoryProjection, error)

func (query branchMemoryQueryFunc) QueryBranchMemory(ctx context.Context, repo domain.ContentHash, branch string, snapshot domain.ContentHash, code string) (domain.MemoryProjection, error) {
	return query(ctx, repo, branch, snapshot, code)
}

func setLiveMemoryFixture(test testing.TB, server *Server, service *app.Service, scope inbound.MemoryReadScope) {
	test.Helper()
	queries, err := service.LiveMemoryQueries()
	if err != nil {
		test.Fatal(err)
	}
	reader := liveMemoryFixtureReader{scope, queries, queries, queries}
	if err := server.SetLiveMemoryQueries(reader); err != nil {
		test.Fatal(err)
	}
}

func TestLiveMemorySetterRejectsAbsentReaderAndPreservesPrevious(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	previousEffective, previousBranch := fixture.server.effectiveMemory, fixture.server.branchMemory
	var missing *memoryquery.LiveQueries
	for _, reader := range []inbound.LiveMemoryQueries{nil, missing} {
		if err := fixture.server.SetLiveMemoryQueries(reader); err == nil || fixture.server.effectiveMemory != previousEffective || fixture.server.branchMemory != previousBranch {
			test.Fatal("absent reader replaced the live query and resource scope", err)
		}
	}
}
