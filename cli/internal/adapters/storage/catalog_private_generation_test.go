package storage

import (
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestCatalogCacheAdvancesPrivateOnlyGenerationWithoutLosingImage(t *testing.T) {
	fixture := catalogCacheTestFixture()
	store := NewFileStore(t.TempDir())
	entries := []domain.CatalogEntry{fixture.protocol(1), fixture.snapshot(t, 1, 2, "retained public snapshot")}
	revision := catalogCacheAppend(t, store, fixture, "", fixture.page("baseline", 2, entries, ""))
	revision = catalogCacheAppend(t, store, fixture, revision, fixture.page("delta", 12, []domain.CatalogEntry{}, ""))
	cache := catalogCacheRead(t, store, fixture)
	if cache.Checkpoint == nil || cache.Checkpoint.Sequence != 12 || !reflect.DeepEqual(cache.Entries, entries) || len(cache.Pending) != 0 {
		t.Fatalf("private-only generation corrupted public image: %+v", cache)
	}
	replacement := fixture.snapshot(t, 1, 20, "later public snapshot")
	catalogCacheAppend(t, store, fixture, revision, fixture.page("delta", 24, []domain.CatalogEntry{replacement}, ""))
	cache = catalogCacheRead(t, store, fixture)
	if cache.Checkpoint == nil || cache.Checkpoint.Sequence != 24 || !reflect.DeepEqual(cache.Entries, []domain.CatalogEntry{entries[0], replacement}) {
		t.Fatalf("sparse public delta lost after checkpoint advance: %+v", cache)
	}
}
