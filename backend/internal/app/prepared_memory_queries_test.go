package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPreparedMemoryQueriesOwnConfiguredReadersWithoutServiceBackreference(test *testing.T) {
	target, cursor := preparedCursorFixture(test)
	storage := &pendingPreparedMemoryStore{requirement: cursor.Requirement}
	service := &Service{meta: storage}
	if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{target.RepoID}); err != nil {
		test.Fatal(err)
	}
	queries, err := service.PreparedMemoryQueries()
	if err != nil {
		test.Fatal(err)
	}
	var _ *memoryquery.PreparedQueries = queries
	service.meta, service.preparedMemoryRepos = nil, nil
	_, err = queries.QueryPreparedMemory(systemTestContext(), target.RepoID, domain.EffectiveMemoryRequest{Selection: target.Selection, Content: target.Content})
	var pending *domain.MemoryReadPending
	if !errors.As(err, &pending) || storage.observed != 1 || storage.read != 1 {
		test.Fatal("prepared query rediscovered the parent or lost configured scope", err, storage.observed, storage.read)
	}
}

func TestPreparedMemoryScopeBindsCurrentAuthorityAndActivePermit(test *testing.T) {
	for _, scenario := range []string{"nested", "callback-replaced", "actor-changed", "foreign-repository", "completed-scope"} {
		test.Run(scenario, func(test *testing.T) {
			scope := &preparedMemoryQueryScope{}
			repo := domain.HashContent([]byte(test.Name()))
			ctx, gate := admissionTestContext(time.Second)
			checks := 0
			ctx = inbound.WithRepositoryActor(ctx, "original")
			ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
				checks++
				actor, _ := inbound.RepositoryActor(bound)
				if requested != repo || actor != "original" || checks > 1 && scenario == "completed-scope" {
					return bound, domain.ErrForbidden
				}
				return bound, nil
			})
			bound, release, err := AcquireResources(ctx, ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer release()
			var previous context.Context
			err = scope.WithMemoryReadResources(bound, repo, func(current context.Context) error {
				previous = current
				if scenario == "completed-scope" {
					return nil
				}
				requested := repo
				switch scenario {
				case "callback-replaced":
					current = inbound.WithDocumentReadAuthorizer(current, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
						checks++
						return bound, domain.ErrForbidden
					})
				case "actor-changed":
					current = inbound.WithRepositoryActor(current, "replacement")
				case "foreign-repository":
					requested = domain.HashContent([]byte("foreign"))
				}
				return scope.WithMemoryReadResources(current, requested, func(context.Context) error { return nil })
			})
			if scenario == "completed-scope" && err == nil {
				err = scope.WithMemoryReadResources(previous, repo, func(context.Context) error { test.Fatal("completed scope retained authority"); return nil })
			}
			if scenario == "nested" {
				if err != nil || checks != 1 {
					test.Fatal("active nested read lost its single authority proof", err, checks)
				}
			} else if !errors.Is(err, domain.ErrForbidden) || checks != 2 {
				test.Fatal("changed authority reused a stale proof", err, checks)
			}
			release()
			assertResourcesReleased(test, gate)
		})
	}
}

func TestPreparedMemoryScopeChecksAfterQueueAndRejectsSnapshotlessPreparation(test *testing.T) {
	scope := &preparedMemoryQueryScope{}
	repo := domain.HashContent([]byte(test.Name()))
	ctx, gate := admissionTestContext(time.Second)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	checks := make(chan struct{}, 1)
	ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		checks <- struct{}{}
		return bound, domain.ErrForbidden
	})
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer release()
	done := make(chan error, 1)
	go func() {
		done <- scope.WithMemoryReadResources(ctx, repo, func(context.Context) error { test.Error("revoked read entered consumer"); return nil })
	}()
	waitResourceQueue(test, gate, ResourceBulk, 1)
	if len(checks) != 0 {
		test.Fatal("authority checked before shared admission")
	}
	release()
	if err := <-done; !errors.Is(err, domain.ErrForbidden) || len(checks) != 1 {
		test.Fatal("queued read retained old authority", err)
	}
	assertResourcesReleased(test, gate)
	if _, err := scope.authorizePreparation(context.Background(), repo); !errors.Is(err, domain.ErrUnauthorized) {
		test.Fatal("unbound caller registered prepared work", err)
	}
}

func TestPreparedMemoryDemandRechecksGeneralAuthorityBeforeRegistration(test *testing.T) {
	target, cursor := preparedCursorFixture(test)
	storage := &preparedMemoryDemandProbe{pendingPreparedMemoryStore: pendingPreparedMemoryStore{requirement: cursor.Requirement}}
	service := &Service{meta: storage}
	if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{target.RepoID}); err != nil {
		test.Fatal(err)
	}
	if err := service.ConfigurePreparedMemoryDemands([]domain.ContentHash{target.RepoID}); err != nil {
		test.Fatal(err)
	}
	queries, err := service.PreparedMemoryQueries()
	if err != nil {
		test.Fatal(err)
	}
	checks := 0
	ctx := inbound.WithDocumentReadAuthorizer(context.Background(), func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		checks++
		if checks == 2 {
			return bound, domain.ErrForbidden
		}
		return bound, nil
	})
	ctx = inbound.WithDocumentSnapshotReadAuthorizer(ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) { return bound, nil })
	_, err = queries.QueryPreparedMemory(ctx, target.RepoID, domain.EffectiveMemoryRequest{Selection: target.Selection, Content: target.Content})
	if !errors.Is(err, domain.ErrForbidden) || checks != 2 || len(storage.requested) != 0 {
		test.Fatal("prepared demand reused read authority", err, checks, storage.requested)
	}
}
