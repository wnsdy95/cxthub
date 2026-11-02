package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type contextBorrowedStore struct {
	*repositoryScopeReadStore
}

func (storage *contextBorrowedStore) InTransaction(ctx context.Context) bool {
	return ctx.Value(repositoryScopeReadKey{}) == storage.repositoryScopeReadStore
}

func TestContextQueriesBorrowedReadUsesOnlySnapshotAuthority(test *testing.T) {
	service, _, storage, repo := newRepositoryScopeQueries(test)
	service.meta = &contextBorrowedStore{repositoryScopeReadStore: storage}
	queries, err := service.ContextQueriesForProfile(inbound.ContextQueryPinnedSnapshots)
	if err != nil {
		test.Fatal(err)
	}
	for _, provided := range []bool{false, true} {
		general, snapshots := 0, 0
		err := storage.WithinReadSnapshot(systemTestContext(), func(bound context.Context) error {
			request := inbound.WithDocumentReadAuthorizer(bound, func(current context.Context, _ domain.ContentHash) (context.Context, error) {
				general++
				return current, domain.ErrIntegrity
			})
			if provided {
				request = inbound.WithDocumentSnapshotReadAuthorizer(request, func(current context.Context, selected domain.ContentHash) (context.Context, error) {
					snapshots++
					if current.Value(repositoryScopeReadKey{}) != storage || selected != repo {
						test.Fatal("borrowed context authority used another snapshot")
					}
					return current, nil
				})
			}
			view, err := queries.QueryContext(request, repo, domain.ContextSelection{})
			if provided {
				if err != nil || view.Version != 1 || snapshots != 1 {
					test.Fatal("snapshot-only context authority was not used", view, err, snapshots)
				}
			} else if !errors.Is(err, domain.ErrCatalogUnsupported) || view.Version != 0 || snapshots != 0 {
				test.Fatal("general-only context authority entered a borrowed read", view, err)
			}
			if general != 0 {
				test.Fatal("general authority executed inside the borrowed read")
			}
			return nil
		})
		if err != nil {
			test.Fatal(err)
		}
	}
}

func TestContextQueriesCompatibilityRechecksGeneralAuthorityWithForeignSnapshotCallback(test *testing.T) {
	service, storage := newFsckSvc(test)
	repo := domain.HashContent([]byte(test.Name()))
	if _, err := storage.PutRepo(systemTestContext(), domain.Repo{ID: repo}); err != nil {
		test.Fatal(err)
	}
	queries, err := service.ContextQueriesForProfile(inbound.ContextQueryCompatibility)
	if err != nil {
		test.Fatal(err)
	}
	general, snapshots := 0, 0
	ctx := inbound.WithDocumentReadAuthorizer(systemTestContext(), func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		general++
		return bound, nil
	})
	ctx = inbound.WithDocumentSnapshotReadAuthorizer(ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		snapshots++
		return bound, domain.ErrIntegrity
	})
	view, err := queries.QueryContext(ctx, repo, domain.ContextSelection{})
	if err != nil || view.Version != 1 || general != 1 || snapshots != 0 {
		test.Fatal("unrelated snapshot callback bypassed compatibility authority", view, err, general, snapshots)
	}
}

func TestContextQueriesOwnBoundSourcesAndAtomicSnapshot(test *testing.T) {
	for _, failure := range []error{nil, domain.ErrConflict} {
		test.Run(map[bool]string{true: "complete", false: "snapshot-failure"}[failure == nil], func(test *testing.T) {
			service, _, storage, repo := newRepositoryScopeQueries(test)
			queries, err := service.ContextQueriesForProfile(inbound.ContextQueryPinnedSnapshots)
			if err != nil {
				test.Fatal(err)
			}
			if _, owned := queries.(*repositoryquery.ContextQueries); !owned {
				test.Fatal("context query is still owned by the parent Service")
			}
			service.meta = &poisonedGraphMetadata{}
			service.blobs = nil
			storage.finishFailure = failure
			view, err := queries.QueryContext(systemTestContext(), repo, domain.ContextSelection{})
			if failure != nil {
				if !errors.Is(err, failure) || !reflect.DeepEqual(view, domain.ContextQueryView{}) {
					test.Fatal("failed snapshot exposed context data", view, err)
				}
			} else if err != nil || view.Revision.Graph != 1 || storage.snapshots != 1 {
				test.Fatal("context query rediscovered the changed parent or lost its snapshot", view, err, storage.snapshots)
			}
		})
	}
}

func TestContextQueriesRejectGeneralOnlyPinnedAuthorization(test *testing.T) {
	service, _, storage, repo := newRepositoryScopeQueries(test)
	queries, err := service.ContextQueriesForProfile(inbound.ContextQueryPinnedSnapshots)
	if err != nil {
		test.Fatal(err)
	}
	ctx := inbound.WithDocumentReadAuthorizer(context.Background(), func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		return bound, nil
	})
	view, err := queries.QueryContext(ctx, repo, domain.ContextSelection{})
	if !errors.Is(err, domain.ErrCatalogUnsupported) || !reflect.DeepEqual(view, domain.ContextQueryView{}) || storage.snapshots != 0 {
		test.Fatal("general authorization bypassed context snapshot authority", view, err, storage.snapshots)
	}
}

func TestContextQueriesRecheckAuthorityInsideSnapshot(test *testing.T) {
	service, _, storage, repo := newRepositoryScopeQueries(test)
	queries, err := service.ContextQueriesForProfile(inbound.ContextQueryPinnedSnapshots)
	if err != nil {
		test.Fatal(err)
	}
	general, pinned := 0, 0
	ctx := inbound.WithDocumentReadAuthorizer(context.Background(), func(bound context.Context, selected domain.ContentHash) (context.Context, error) {
		general++
		if bound.Value(repositoryScopeReadKey{}) != nil || selected != repo {
			test.Fatal("general authority entered the context snapshot")
		}
		return inbound.WithDocumentSnapshotReadAuthorizer(bound, func(snapshot context.Context, selected domain.ContentHash) (context.Context, error) {
			pinned++
			if snapshot.Value(repositoryScopeReadKey{}) != storage || selected != repo {
				test.Fatal("context authority did not use the owned read snapshot")
			}
			return snapshot, domain.ErrForbidden
		}), nil
	})
	view, err := queries.QueryContext(ctx, repo, domain.ContextSelection{})
	if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(view, domain.ContextQueryView{}) || general != 1 || pinned != 1 || storage.metadataReads != 0 {
		test.Fatal("revoked context authority reached payload", view, err, general, pinned, storage.metadataReads)
	}
}
