package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryDependencySourceStore struct {
	*effectiveMemorySourceStore
	generation domain.RepositoryMemorySourceGeneration
	err        error
	reads      int
}

func (storage *memoryDependencySourceStore) ReadRepositoryMemorySourceGeneration(context.Context, domain.ContentHash) (domain.RepositoryMemorySourceGeneration, error) {
	storage.reads++
	return storage.generation, storage.err
}

func TestEffectiveMemoryDependencyGenerationFencesContinuation(test *testing.T) {
	fixture := newEffectiveFixture(test)
	graph := testProjectionGeneration(fixture.repo)
	storage := &memoryDependencySourceStore{
		effectiveMemorySourceStore: &effectiveMemorySourceStore{FSStore: fixture.st, source: graph, readOnly: true},
		generation: domain.RepositoryMemorySourceGeneration{
			Version: 1, Graph: graph, Epoch: "12345678-abcd-4321-9876-123456789abc", Sequence: 1,
		},
	}
	fixture.svc.meta = storage
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: fixture.root, CodeCommit: effectiveOID(3)}, Limit: 1}
	ctx := systemTestContext()
	before, err := fixture.svc.QueryEffectiveMemory(ctx, fixture.repo, request)
	if err != nil || before.NextCursor == "" || before.DeliveryStateHash == "" {
		test.Fatal("fixture needs a complete proof and continuation", err)
	}
	for _, mutate := range []func(){
		func() { storage.generation.Sequence++ },
		func() {
			storage.generation.Epoch = "23456789-abcd-4321-9876-123456789abc"
			storage.generation.Sequence = 0
		},
	} {
		mutate()
		after, err := fixture.svc.QueryEffectiveMemory(ctx, fixture.repo, request)
		if err != nil {
			test.Fatal(err)
		}
		if after.StateHash == before.StateHash || after.Revision != before.Revision || after.LineageHash != before.LineageHash {
			test.Fatal("memory-only generation did not independently fence continuation")
		}
		if after.DeliveryStateHash != before.DeliveryStateHash || !reflect.DeepEqual(after.Items, before.Items) {
			test.Fatal("unchanged delivery semantics depend on a dependency clock")
		}
		request.Cursor = before.NextCursor
		if _, err := fixture.svc.QueryEffectiveMemory(ctx, fixture.repo, request); !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
			test.Fatal("accepted a mixed memory dependency generation", err)
		}
		request.Cursor = ""
		before = after
	}
}

func TestEffectiveMemoryDependencyFailureCannotReuseProof(test *testing.T) {
	for _, name := range []string{"epoch", "negative sequence", "foreign repo", "graph sequence", "version", "storage error"} {
		test.Run(name, func(test *testing.T) {
			fixture := newEffectiveFixture(test)
			graph := testProjectionGeneration(fixture.repo)
			storage := &memoryDependencySourceStore{
				effectiveMemorySourceStore: &effectiveMemorySourceStore{FSStore: fixture.st, source: graph, readOnly: true},
				generation:                 domain.RepositoryMemorySourceGeneration{Version: 1, Graph: graph, Epoch: "12345678-abcd-4321-9876-123456789abc"},
			}
			fixture.svc.meta = storage
			before := fixture.query(test, 3)
			want := domain.ErrIntegrity
			switch name {
			case "epoch":
				storage.generation.Epoch = ""
			case "negative sequence":
				storage.generation.Sequence = -1
			case "foreign repo":
				storage.generation.Graph.RepoID = hh("foreign")
			case "graph sequence":
				storage.generation.Graph.Sequence++
			case "version":
				storage.generation.Version++
			case "storage error":
				want = errors.New("source unavailable")
				storage.err = want
			}
			entries := fixture.svc.deliveryCache.Len()
			_, err := fixture.svc.QueryEffectiveMemory(systemTestContext(), fixture.repo, domain.EffectiveMemoryRequest{Selection: before.Selection})
			if !errors.Is(err, want) || fixture.svc.deliveryCache.Len() != entries {
				test.Fatal("source failure reused or published cached proof", err)
			}
		})
	}
}

func TestMemoryDependencyGenerationRequiresCurrentReadSnapshot(test *testing.T) {
	fixture := newEffectiveFixture(test)
	graph := testProjectionGeneration(fixture.repo)
	storage := &memoryDependencySourceStore{
		effectiveMemorySourceStore: &effectiveMemorySourceStore{FSStore: fixture.st, source: graph, readOnly: false},
		generation:                 domain.RepositoryMemorySourceGeneration{Version: 1, Graph: graph, Epoch: "12345678-abcd-4321-9876-123456789abc"},
	}
	fixture.svc.meta = storage
	for _, ctx := range []context.Context{
		systemTestContext(),
		context.WithValue(systemTestContext(), afterCommitKey{}, &afterCommitActions{}),
	} {
		generation, memory, err := fixture.svc.memorySourceGeneration(ctx, fixture.repo)
		if err != nil || memory != nil || generation.Epoch != "" || storage.reads != 0 {
			test.Fatal("read generation trusted unknown/write scope", err)
		}
		storage.readOnly = true
	}
}
