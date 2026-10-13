package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestRootEventsFSCurrentOwnershipAndOwnedPage(test *testing.T) {
	ctx, dir := context.Background(), test.TempDir()
	storage := NewFSStore(dir)
	repo := domain.HashContent([]byte("owned root events"))
	fixture := rootFixture(test, strings.Repeat("selected payload", 100000))
	seedRootFS(test, storage, repo, fixture)
	before := rootFSImage(test, dir)
	var retained domain.VerifiedRootEvents
	for _, selection := range []domain.RootEventSelection{{Limit: 1}, {Offset: 1, Limit: 1}, {HashesOnly: true}} {
		projection, err := storage.ReadVerifiedRootEvents(ctx, repo, fixture.hash, selection)
		if err != nil || projection.Reference().DocumentRef() != (domain.DocumentRef{Hash: fixture.hash, Identity: domain.DocumentIdentityRootV1}) {
			test.Fatal("owned root projection", err)
		}
		if !selection.HashesOnly && selection.Offset == 0 {
			retained = projection
		}
	}
	if !reflect.DeepEqual(before, rootFSImage(test, dir)) {
		test.Fatal("projection wrote bytes, indexes or metadata")
	}
	if _, err := storage.ReadVerifiedRootEvents(ctx, domain.HashContent([]byte("foreign repo")), fixture.hash, domain.RootEventSelection{Limit: 1}); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("foreign root admitted", err)
	}
	chunk := fixture.manifest.Chunks[len(fixture.manifest.Chunks)-1].Hash
	if err := os.WriteFile(storage.chunkPath(repo, chunk), []byte("corrupt current chunk"), 0600); err != nil {
		test.Fatal(err)
	}
	page, err := retained.Page(ctx)
	if err != nil || !reflect.DeepEqual(page.Events, fixture.cir.Events) {
		test.Fatal("selected bytes reread after verification", err)
	}
	for _, selection := range []domain.RootEventSelection{{Limit: 1}, {Offset: 1, Limit: 1}, {HashesOnly: true}} {
		projection, err := storage.ReadVerifiedRootEvents(ctx, repo, fixture.hash, selection)
		if !errors.Is(err, domain.ErrIntegrity) || projection.Reference().Valid() {
			test.Fatal("warm projection trusted changed tail", err)
		}
	}
	if err := os.Remove(storage.chunkPath(repo, chunk)); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.ReadVerifiedRootEvents(ctx, repo, fixture.hash, domain.RootEventSelection{Offset: 1, Limit: 1}); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("missing tail accepted on empty page", err)
	}
}
