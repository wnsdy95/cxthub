//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGMemorySnapshotMethodsRecheckCurrentSnapshotAuthority(test *testing.T) {
	service, storage, _, repo, request, _ := qualifiedMemoryFixturePG(test)
	snapshot, err := storage.GetSnapshot(systemTestContext(), repo, request.Selection.SnapshotID)
	if err != nil {
		test.Fatal(err)
	}
	reads := map[string]func(context.Context) (bool, error){
		"object": func(ctx context.Context) (bool, error) {
			result, err := service.GetMemoryObject(ctx, repo, snapshot.MemoryHash)
			return !reflect.DeepEqual(result, domain.MemoryDigest{}), err
		},
		"digest": func(ctx context.Context) (bool, error) {
			result, err := service.GetMemoryDigest(ctx, repo, snapshot.ID)
			return !reflect.DeepEqual(result, domain.MemoryDigest{}), err
		},
		"projection": func(ctx context.Context) (bool, error) {
			result, err := service.GetMemoryProjection(ctx, repo, snapshot.ID)
			return !reflect.DeepEqual(result, domain.MemoryProjection{}), err
		},
	}
	assertCurrentMemorySnapshotAuthority(test, storage, repo, reads)
}

func TestPGMemoryLiveMethodsRecheckCurrentSnapshotAuthority(test *testing.T) {
	service, storage, _, repo, request, _ := qualifiedMemoryFixturePG(test)
	reads := map[string]func(context.Context) (bool, error){
		"effective": func(ctx context.Context) (bool, error) {
			result, err := service.QueryEffectiveMemory(ctx, repo, request)
			return !reflect.DeepEqual(result, domain.EffectiveMemoryPage{}), err
		},
		"branch": func(ctx context.Context) (bool, error) {
			result, err := service.QueryBranchMemory(ctx, repo, "main", request.Selection.SnapshotID, request.Selection.CodeCommit)
			return !reflect.DeepEqual(result, domain.MemoryProjection{}), err
		},
		"positions": func(ctx context.Context) (bool, error) {
			result, err := service.QueryMemoryPositions(ctx, repo, request.Selection.SnapshotID, "")
			return !reflect.DeepEqual(result, domain.MemoryPositions{}), err
		},
	}
	assertCurrentMemorySnapshotAuthority(test, storage, repo, reads)
}

func assertCurrentMemorySnapshotAuthority(test *testing.T, storage interface {
	InTransaction(context.Context) bool
	InReadOnlyTransaction(context.Context) bool
}, repo domain.ContentHash, reads map[string]func(context.Context) (bool, error)) {
	test.Helper()
	for name, read := range reads {
		test.Run(name, func(test *testing.T) {
			currentChecks := 0
			ctx := inbound.WithRepositoryActor(context.Background(), "reader")
			ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, selected domain.ContentHash) (context.Context, error) {
				if storage.InTransaction(bound) || selected != repo {
					test.Fatal("general authentication ran in the payload transaction")
				}
				return bound, nil
			})
			ctx = inbound.WithDocumentSnapshotReadAuthorizer(ctx, func(bound context.Context, selected domain.ContentHash) (context.Context, error) {
				currentChecks++
				if !storage.InReadOnlyTransaction(bound) || selected != repo {
					test.Fatal("current authority escaped the payload snapshot")
				}
				return bound, domain.ErrForbidden
			})
			payload, err := read(ctx)
			if !errors.Is(err, domain.ErrForbidden) || payload || currentChecks != 1 {
				test.Fatal("memory payload ignored current snapshot authority", err, payload, currentChecks)
			}
		})
	}
}
