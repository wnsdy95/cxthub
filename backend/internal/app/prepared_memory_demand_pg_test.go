//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGPreparedMemoryDemandCompletesWithoutManualRegistration(test *testing.T) {
	service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	memoryWorkConfigurePG(test, service, storage, repo)
	if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if err := service.ConfigurePreparedMemoryDemands([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	before, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	expected, err := service.QueryEffectiveMemory(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	_, err = service.QueryPreparedMemory(ctx, repo, request)
	var pending *domain.MemoryReadPending
	if !errors.As(err, &pending) || pending.Cursor == "" {
		test.Fatal("cold read failed to register retry", err)
	}
	initial, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil || initial.State != "waiting" {
		test.Fatal("read did not durably register work", err)
	}
	request.Cursor = pending.Cursor
	for range 3 {
		_, err := service.QueryPreparedMemory(ctx, repo, request)
		if !errors.As(err, &pending) || pending.Cursor != request.Cursor {
			test.Fatal("retry changed source floor", err)
		}
	}
	status, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil || !reflect.DeepEqual(status, initial) {
		test.Fatal("duplicate requests rewrote waiting work", err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal(err)
	}
	actual, err := service.QueryPreparedMemory(ctx, repo, request)
	if err != nil || !actual.Status.Complete {
		test.Fatal("automatically requested work never became readable", err)
	}
	actual.Page.NextCursor, expected.NextCursor = "", ""
	if !reflect.DeepEqual(actual.Page, expected) {
		test.Fatal("automatic preparation changed memory content")
	}
	after, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if err != nil || after != before {
		test.Fatal("preparation changed business source", err)
	}
	var count int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM memory_read_work WHERE repo_id=$1`, repo).Scan(&count); err != nil || count != 1 {
		test.Fatal("requests duplicated preparation targets", count, err)
	}
}

func TestPGPreparedMemoryDemandReauthorizesOutsideReadSnapshot(test *testing.T) {
	for _, revoke := range []bool{false, true} {
		test.Run(map[bool]string{false: "authorized", true: "revoked"}[revoke], func(test *testing.T) {
			service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
			memoryWorkConfigurePG(test, service, storage, repo)
			if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{repo}); err != nil {
				test.Fatal(err)
			}
			if err := service.ConfigurePreparedMemoryDemands([]domain.ContentHash{repo}); err != nil {
				test.Fatal(err)
			}
			checks, generalChecks := 0, 0
			ctx := inbound.WithRepositoryActor(context.Background(), "reader")
			ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
				actor, system := inbound.RepositoryActor(bound)
				if storage.InTransaction(bound) || requested != repo || actor != "reader" || system {
					test.Fatal("preparation general authority escaped its current caller or ran inside a transaction")
				}
				generalChecks++
				return bound, nil
			})
			ctx = inbound.WithDocumentSnapshotReadAuthorizer(ctx, func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
				actor, system := inbound.RepositoryActor(bound)
				if !storage.InReadOnlyTransaction(bound) || requested != repo || actor != "reader" || system {
					test.Fatal("authorization did not use a real read snapshot")
				}
				checks++
				if revoke && checks == 3 {
					return bound, domain.ErrForbidden
				}
				return bound, nil
			})
			_, err := service.QueryPreparedMemory(ctx, repo, request)
			var count int
			if failure := connection.QueryRow(systemTestContext(), `SELECT count(*) FROM memory_read_work WHERE repo_id=$1`, repo).Scan(&count); failure != nil {
				test.Fatal(failure)
			}
			if revoke {
				if !errors.Is(err, domain.ErrForbidden) || count != 0 {
					test.Fatal("revoked read queued work", count, err)
				}
			} else {
				var pending *domain.MemoryReadPending
				if !errors.As(err, &pending) || count != 1 {
					test.Fatal("authorized read failed to enqueue outside snapshot", count, err)
				}
				if err := service.ProcessMemoryReadPreparations(systemTestContext(), 1); err != nil {
					test.Fatal(err)
				}
				ctx = inbound.WithDocumentSnapshotReadAuthorizer(ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
					return bound, domain.ErrForbidden
				})
				request.Cursor = pending.Cursor
				if _, err := service.QueryPreparedMemory(ctx, repo, request); !errors.Is(err, domain.ErrForbidden) {
					test.Fatal("queued read authority survived revocation", err)
				}
			}
			if checks != 3 || generalChecks != 1 {
				test.Fatal("missing fresh authorization before demand", checks, generalChecks)
			}
		})
	}
}

func TestPGPreparedMemoryDemandReadOnlyNeverEnqueues(test *testing.T) {
	service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	memoryWorkConfigurePG(test, service, storage, repo)
	if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if err := service.ConfigurePreparedMemoryDemands([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if _, err := service.QueryPreparedMemory(outbound.WithDocReadOnly(ctx), repo, request); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("read-only composition requested writes", err)
	}
	var count int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM memory_read_work WHERE repo_id=$1`, repo).Scan(&count); err != nil || count != 0 {
		test.Fatal("read-only request registered work", count, err)
	}
}

func TestPGPreparedMemoryDemandPreservesRootIdentityCompatibility(test *testing.T) {
	service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	memoryWorkConfigurePG(test, service, storage, repo)
	if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if err := service.ConfigurePreparedMemoryDemands([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal(err)
	}
	if _, err := service.QueryPreparedMemory(ctx, repo, request); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		test.Fatal("legacy client bypassed required identity", err)
	}
	var count int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM memory_read_work WHERE repo_id=$1`, repo).Scan(&count); err != nil || count != 0 {
		test.Fatal("incompatible read enqueued work", count, err)
	}
	ctx = inbound.WithDocumentIdentities(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	_, err := service.QueryPreparedMemory(ctx, repo, request)
	var pending *domain.MemoryReadPending
	if !errors.As(err, &pending) {
		test.Fatal("compatible cold read lost identity declaration during registration", err)
	}
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM memory_read_work WHERE repo_id=$1`, repo).Scan(&count); err != nil || count != 1 {
		test.Fatal("compatible read failed to register work", count, err)
	}
}
