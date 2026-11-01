package app

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestMemorySnapshotQueriesKeepReadersWithoutParentBackreference(test *testing.T) {
	fixture := newEffectiveFixture(test)
	queries, err := fixture.svc.MemorySnapshotQueries()
	if err != nil {
		test.Fatal(err)
	}
	fixture.svc.meta, fixture.svc.blobs = nil, nil
	digest, err := queries.GetMemoryDigest(systemTestContext(), fixture.repo, fixture.root)
	if err != nil || digest.SnapshotID != fixture.root {
		test.Fatal("memory snapshot query rediscovered its parent", digest, err)
	}
}

func TestMemoryResourceIdentityIsIndependentAndInitializedOnce(test *testing.T) {
	service := &Service{}
	owners := make(chan *memoryReadOwner, 32)
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			owners <- service.memoryResourceScope().ownerKey
		}()
	}
	workers.Wait()
	close(owners)
	first := service.memoryResourceScope().ownerKey
	for owner := range owners {
		if owner == nil || owner != first {
			test.Fatal("resource scopes did not share one independent identity")
		}
	}
	other := (&Service{}).memoryResourceScope().ownerKey
	if first == other {
		test.Fatal("different providers share a memory resource identity")
	}
}

func TestMemoryResourceScopesDoNotConfuseSnapshotAndGeneralAuthority(test *testing.T) {
	for _, legacyFirst := range []bool{false, true} {
		test.Run(map[bool]string{false: "snapshot-cannot-authorize-legacy", true: "legacy-can-nest-current-snapshot"}[legacyFirst], func(test *testing.T) {
			key := &memoryReadOwner{}
			snapshot := &preparedMemoryQueryScope{ownerKey: key, snapshots: true}
			legacy := &preparedMemoryQueryScope{ownerKey: key, snapshots: true, forceGeneral: true}
			checks := 0
			ctx := inbound.WithDocumentReadAuthorizer(context.Background(), func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
				checks++
				if !legacyFirst {
					return bound, domain.ErrForbidden
				}
				return bound, nil
			})
			ctx = inbound.WithDocumentSnapshotReadAuthorizer(ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) { return bound, nil })
			outer, inner := snapshot, legacy
			if legacyFirst {
				outer, inner = legacy, snapshot
			}
			err := outer.WithMemoryReadResources(ctx, domain.HashContent([]byte(test.Name())), func(bound context.Context) error {
				return inner.WithMemoryReadResources(bound, domain.HashContent([]byte(test.Name())), func(context.Context) error { return nil })
			})
			if legacyFirst && err != nil || !legacyFirst && !errors.Is(err, domain.ErrForbidden) || checks != 1 {
				test.Fatal("resource memo confused authority kinds or duplicated admission", err, checks)
			}
		})
	}
}
