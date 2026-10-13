//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestRootPGEventProjectionOwnedPageAndNoWrites(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	repo, namespace := rootPGRepo(test, ctx, storage)
	fixture := uniqueRootPublicationFixturePG(test, strings.Repeat("owned page ", 60000))
	seedRootPG(test, ctx, storage, repo, fixture)
	before := rootPGImage(test, ctx, storage, repo, namespace)
	selection := domain.RootEventSelection{Offset: 0, Limit: 1}
	projection, err := storage.ReadVerifiedRootEvents(ctx, repo, fixture.hash, selection)
	if err != nil {
		test.Fatal(err)
	}
	page, err := projection.Page(ctx)
	if err != nil || page.Hash != fixture.hash || page.Total != 1 || page.Offset != 0 || page.Next != -1 || !reflect.DeepEqual(page.Events, fixture.cir.Events) {
		test.Fatal("root event page changed", err)
	}
	if projection.Reference().DocumentRef() != (domain.DocumentRef{Hash: fixture.hash, Identity: domain.DocumentIdentityRootV1}) {
		test.Fatal("projection lost its verified root identity")
	}
	hashes, err := storage.ReadVerifiedRootEvents(ctx, repo, fixture.hash, domain.RootEventSelection{HashesOnly: true})
	if err != nil {
		test.Fatal(err)
	}
	first, err := hashes.EventHashes()
	if err != nil || len(first) != 1 {
		test.Fatal("missing verified base hashes", err)
	}
	first[0] = domain.HashContent([]byte("changed caller copy"))
	second, err := hashes.EventHashes()
	if err != nil || second[0] == first[0] {
		test.Fatal("base hashes alias caller memory", err)
	}
	if _, err := hashes.Page(ctx); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("hash-only projection returned body", err)
	}
	if after := rootPGImage(test, ctx, storage, repo, namespace); after != before {
		test.Fatal("event projection wrote persistent state")
	}
	tail := fixture.manifest.Chunks[len(fixture.manifest.Chunks)-1].Hash
	original := docCompress(fixture.bodies[tail])
	test.Cleanup(func() {
		if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, tail, original); err != nil {
			test.Error(err)
		}
	})
	corrupt := bytes.Clone(fixture.bodies[tail])
	corrupt[len(corrupt)-1] ^= 1
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, tail, docCompress(corrupt)); err != nil {
		test.Fatal(err)
	}
	retained, err := projection.Page(ctx)
	if err != nil || !reflect.DeepEqual(retained, page) {
		test.Fatal("verified page reread changed storage", err)
	}
	if current, err := storage.ReadVerifiedRootEvents(ctx, repo, fixture.hash, selection); !errors.Is(err, domain.ErrIntegrity) || current.Reference().Valid() {
		test.Fatal("new request trusted previous tail proof", err)
	}
}

func TestRootPGEventProjectionCurrentOwnership(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	repo, _ := rootPGRepo(test, ctx, storage)
	fixture := uniqueRootPublicationFixturePG(test, strings.Repeat("repository scoped ", 40000))
	seedRootPG(test, ctx, storage, repo, fixture)
	selection := domain.RootEventSelection{Offset: 0, Limit: 1}
	if _, err := storage.ReadVerifiedRootEvents(ctx, repo, fixture.hash, selection); err != nil {
		test.Fatal(err)
	}
	other, _ := rootPGRepo(test, ctx, storage)
	if projection, err := storage.ReadVerifiedRootEvents(ctx, other, fixture.hash, selection); !errors.Is(err, domain.ErrNotFound) || projection.Reference().Valid() {
		test.Fatal("foreign repository read shared object", err)
	}
	tail := fixture.manifest.Chunks[len(fixture.manifest.Chunks)-1].Hash
	if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, tail); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if _, err := storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2) ON CONFLICT DO NOTHING`, repo, tail); err != nil {
			test.Error(err)
		}
	})
	for _, requested := range []domain.RootEventSelection{selection, {HashesOnly: true}, {Offset: 1, Limit: 1}} {
		if projection, err := storage.ReadVerifiedRootEvents(ctx, repo, fixture.hash, requested); !errors.Is(err, domain.ErrNotFound) || projection.Reference().Valid() {
			test.Fatal("projection ignored revoked tail ownership", err)
		}
	}
}

func TestRootPGEventProjectionSameReadSnapshot(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	repo, _ := rootPGRepo(test, ctx, storage)
	fixture := uniqueRootPublicationFixturePG(test, strings.Repeat("stable read ", 50000))
	seedRootPG(test, ctx, storage, repo, fixture)
	tail := fixture.manifest.Chunks[len(fixture.manifest.Chunks)-1].Hash
	original := docCompress(fixture.bodies[tail])
	test.Cleanup(func() {
		if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, tail, original); err != nil {
			test.Error(err)
		}
	})
	selection := domain.RootEventSelection{Offset: 0, Limit: 1}
	err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		first, err := storage.ReadVerifiedRootEvents(bound, repo, fixture.hash, selection)
		if err != nil {
			return err
		}
		corrupt := bytes.Clone(fixture.bodies[tail])
		corrupt[len(corrupt)-1] ^= 1
		if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, tail, docCompress(corrupt)); err != nil {
			return err
		}
		second, err := storage.ReadVerifiedRootEvents(bound, repo, fixture.hash, selection)
		if err != nil {
			return err
		}
		before, err := first.Page(bound)
		if err != nil {
			return err
		}
		after, err := second.Page(bound)
		if err == nil && !reflect.DeepEqual(before, after) {
			test.Error("nested root projection escaped the read snapshot")
		}
		return err
	})
	if err != nil {
		test.Fatal(err)
	}
	if projection, err := storage.ReadVerifiedRootEvents(ctx, repo, fixture.hash, selection); !errors.Is(err, domain.ErrIntegrity) || projection.Reference().Valid() {
		test.Fatal("new transaction missed current corruption", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if projection, err := storage.ReadVerifiedRootEvents(canceled, repo, fixture.hash, selection); !errors.Is(err, context.Canceled) || projection.Reference().Valid() {
		test.Fatal("canceled read returned a projection", err)
	}
}
