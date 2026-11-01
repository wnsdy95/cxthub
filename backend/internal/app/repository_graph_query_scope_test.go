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

func TestRepositoryGraphQueriesKeepBoundSourcesAndAtomicSnapshotResult(test *testing.T) {
	for _, failFinish := range []bool{false, true} {
		test.Run(map[bool]string{false: "complete", true: "snapshot-failure"}[failFinish], func(test *testing.T) {
			service, _, storage, repo := newRepositoryScopeQueries(test)
			queries, err := service.GraphQueriesForProfile(inbound.RepositoryGraphPinnedSnapshots)
			if err != nil {
				test.Fatal(err)
			}
			if _, owned := queries.(*repositoryquery.GraphQueries); !owned {
				test.Fatal("graph endpoint still belongs to the parent Service")
			}
			service.meta = &poisonedGraphMetadata{}
			service.graphReadRepos = map[domain.ContentHash]bool{repo: true}
			failure := errors.New("graph snapshot completion failed")
			if failFinish {
				storage.finishFailure = failure
			}
			view, err := queries.GetRepositoryView(systemTestContext(), repo)
			if failFinish {
				if !errors.Is(err, failure) || !reflect.DeepEqual(view, domain.RepositoryView{}) {
					test.Fatal("snapshot failure leaked a partial graph result", view, err)
				}
			} else if err != nil || view.Graph == nil || view.DefaultBranch != "main" || view.Revision.Graph != 1 || storage.snapshots != 1 {
				test.Fatal("query rediscovered a changed parent or lost coherent inputs", view, err, storage.snapshots)
			}
		})
	}
}

func TestRepositoryGraphQueriesRequireSnapshotAuthorizationAfterGeneralRead(test *testing.T) {
	service, _, storage, repo := newRepositoryScopeQueries(test)
	queries, err := service.GraphQueriesForProfile(inbound.RepositoryGraphPinnedSnapshots)
	if err != nil {
		test.Fatal(err)
	}
	ctx := inbound.WithDocumentReadAuthorizer(context.Background(), func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		return bound, nil
	})
	view, err := queries.GetRepositoryView(ctx, repo)
	if !errors.Is(err, domain.ErrCatalogUnsupported) || !reflect.DeepEqual(view, domain.RepositoryView{}) || storage.snapshots != 0 {
		test.Fatal("general-only authority entered the graph snapshot", view, err, storage.snapshots)
	}
}

func TestRepositoryGraphQueriesRecheckCurrentAuthorityInsideSnapshot(test *testing.T) {
	service, _, storage, repo := newRepositoryScopeQueries(test)
	queries, err := service.GraphQueriesForProfile(inbound.RepositoryGraphPinnedSnapshots)
	if err != nil {
		test.Fatal(err)
	}
	general, pinned := 0, 0
	ctx := inbound.WithDocumentReadAuthorizer(context.Background(), func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		general++
		return inbound.WithDocumentSnapshotReadAuthorizer(bound, func(snapshot context.Context, selected domain.ContentHash) (context.Context, error) {
			pinned++
			if snapshot.Value(repositoryScopeReadKey{}) != storage || selected != repo {
				test.Fatal("graph authority was not checked in its owned snapshot")
			}
			return snapshot, domain.ErrForbidden
		}), nil
	})
	graph, err := queries.QueryGraphState(ctx, repo, "")
	if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(graph, domain.GraphState{}) || general != 1 || pinned != 1 || storage.metadataReads != 0 {
		test.Fatal("revoked graph authority reached metadata", graph, err, general, pinned, storage.metadataReads)
	}
}
