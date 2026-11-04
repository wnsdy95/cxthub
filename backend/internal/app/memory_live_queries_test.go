package app

import (
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestLiveMemoryQueriesKeepCapturedReadersWithoutParentBackreference(test *testing.T) {
	fixture := newEffectiveFixture(test)
	queries, err := fixture.svc.LiveMemoryQueries()
	if err != nil {
		test.Fatal(err)
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: fixture.root, CodeCommit: effectiveOID(3)}, Limit: 1}
	before, err := queries.QueryEffectiveMemory(systemTestContext(), fixture.repo, request)
	if err != nil {
		test.Fatal(err)
	}
	fixture.svc.meta, fixture.svc.blobs, fixture.svc.projectionEvidence, fixture.svc.projectionMemory = nil, nil, nil, nil
	after, err := queries.QueryEffectiveMemory(systemTestContext(), fixture.repo, request)
	if err != nil || !reflect.DeepEqual(before, after) {
		test.Fatal("live query rediscovered its parent or changed the captured reader binding", err, before, after)
	}
}
