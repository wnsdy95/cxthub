package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type effectiveMemorySourceStore struct {
	*store.FSStore
	source   domain.RepositoryViewSourceCheckpoint
	readOnly bool
	err      error
}

func (storage *effectiveMemorySourceStore) InReadOnlyTransaction(context.Context) bool {
	return storage.readOnly
}

func (storage *effectiveMemorySourceStore) ReadRepositoryGraphSourceGeneration(context.Context, domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error) {
	return storage.source, storage.err
}

func TestEffectiveMemorySourceSeparatesCursorAndDeliveryIdentity(test *testing.T) {
	fixture := newEffectiveFixture(test)
	storage := &effectiveMemorySourceStore{FSStore: fixture.st, source: testProjectionGeneration(fixture.repo), readOnly: true}
	fixture.svc.meta = storage
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: fixture.root, CodeCommit: effectiveOID(3)}, Limit: 1}
	ctx := systemTestContext()
	before, err := fixture.svc.QueryEffectiveMemory(ctx, fixture.repo, request)
	if err != nil || before.NextCursor == "" {
		test.Fatal("fixture needs paginated memory", err)
	}
	if _, cached := fixture.svc.deliveryCache.Lookup(before.StateHash); !cached {
		test.Fatal("current source did not cache delivery proof")
	}
	for _, change := range []func(){
		func() { storage.source.Sequence++ },
		func() { storage.source.Epoch = "23456789-abcd-4321-9876-123456789abc"; storage.source.Sequence = 0 },
	} {
		change()
		after, err := fixture.svc.QueryEffectiveMemory(ctx, fixture.repo, request)
		if err != nil {
			test.Fatal(err)
		}
		if after.StateHash == before.StateHash || after.Revision != before.Revision || after.LineageHash != before.LineageHash {
			test.Fatal("source change did not independently fence continuation")
		}
		if after.DeliveryStateHash != before.DeliveryStateHash || !reflect.DeepEqual(after.Items, before.Items) {
			test.Fatal("unrelated source activity changed selected delivery semantics")
		}
		continuation := request
		continuation.Cursor = before.NextCursor
		if _, err := fixture.svc.QueryEffectiveMemory(ctx, fixture.repo, continuation); !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
			test.Fatal("continuation crossed source sequence/incarnation", err)
		}
		before = after
	}
}

func TestEffectiveMemorySourceFailureNeverReusesProof(test *testing.T) {
	for _, name := range []string{"foreign repository", "wrong scope", "invalid epoch", "query failure", "integrity failure"} {
		test.Run(name, func(test *testing.T) {
			fixture := newEffectiveFixture(test)
			storage := &effectiveMemorySourceStore{FSStore: fixture.st, source: testProjectionGeneration(fixture.repo), readOnly: true}
			fixture.svc.meta = storage
			before := fixture.query(test, 3)
			wanted := domain.ErrIntegrity
			switch name {
			case "foreign repository":
				storage.source.RepoID = hh("foreign")
			case "wrong scope":
				storage.source.Scope = domain.RepositoryViewSourceScope
			case "invalid epoch":
				storage.source.Epoch = ""
			case "query failure":
				wanted = errors.New("source unavailable")
				storage.err = wanted
			case "integrity failure":
				storage.err = wanted
			}
			entries := fixture.svc.deliveryCache.Len()
			_, err := fixture.svc.QueryEffectiveMemory(systemTestContext(), fixture.repo, domain.EffectiveMemoryRequest{Selection: before.Selection})
			if !errors.Is(err, wanted) || fixture.svc.deliveryCache.Len() != entries {
				test.Fatal("source error returned or published a cached result", err)
			}
		})
	}
}

func TestEffectiveMemoryWithoutSourceDoesNotCache(test *testing.T) {
	for _, name := range []string{"legacy", "write", "not found", "unsupported", "reset"} {
		test.Run(name, func(test *testing.T) {
			fixture := newEffectiveFixture(test)
			storage := &effectiveMemorySourceStore{FSStore: fixture.st, source: testProjectionGeneration(fixture.repo), readOnly: true}
			fixture.svc.meta = storage
			switch name {
			case "legacy":
				fixture.svc.meta = fixture.st
			case "write":
				storage.readOnly = false
			case "not found":
				storage.err = domain.ErrNotFound
			case "unsupported":
				storage.err = domain.ErrCatalogUnsupported
			case "reset":
				storage.err = domain.ErrCatalogResetRequired
			}
			before := fixture.query(test, 3)
			if before.Revision.Graph == 0 && before.Revision.Evidence == 0 {
				test.Fatal("fixture needs legacy clocks")
			}
			fixture.svc.deliveryCache.Store(before.StateHash, hh("must not reuse"))
			after := fixture.query(test, 3)
			if after.DeliveryStateHash != before.DeliveryStateHash {
				test.Fatal("unproven source reused process-wide proof")
			}
			fixture.svc.deliveryCache = effectiveMemoryDeliveryCache{}
			fixture.query(test, 3)
			if fixture.svc.deliveryCache.Len() != 0 {
				test.Fatal("unproven source published process-wide proof")
			}
		})
	}
}
