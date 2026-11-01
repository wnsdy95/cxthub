//go:build postgres

package app

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type countedMemoryPreparationStore struct {
	*store.PostgresStore
	builds int
}

func (storage *countedMemoryPreparationStore) BuildGraphSourceReader(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, repo domain.Repo, entries []domain.RepositoryViewSourceEntry) (outbound.GraphEvidenceReader, error) {
	storage.builds++
	return storage.PostgresStore.BuildGraphSourceReader(ctx, checkpoint, repo, entries)
}

func TestPGMemoryPreparationBuildsGraphReaderOnce(test *testing.T) {
	service, storage, _, repo, request, _ := qualifiedMemoryFixturePG(test)
	counted := &countedMemoryPreparationStore{PostgresStore: storage}
	service.meta = counted
	ctx := systemTestContext()
	prepared, err := service.PrepareMemoryRead(ctx, repo, request, nil)
	if err != nil || counted.builds != 1 {
		test.Fatalf("preparation graph reader builds = %d, want 1: %v", counted.builds, err)
	}
	replayed, err := BuildMemoryRead(ctx, prepared.input, counted)
	if err != nil || !reflect.DeepEqual(replayed, prepared.output) || counted.builds != 2 {
		test.Fatal("direct preparation differs from complete persisted replay", err)
	}
	identifier, err := service.PublishPreparedMemoryRead(ctx, prepared, time.Now().Add(time.Hour))
	if err != nil {
		test.Fatal(err)
	}
	want, err := service.QueryEffectiveMemory(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	actual, err := service.QueryQualifiedMemory(ctx, repo, identifier, request)
	if err != nil || !reflect.DeepEqual(actual, want) {
		test.Fatal("qualified direct preparation changed the query result", err)
	}
}
