//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGPreparedMemoryBorrowedSnapshotNeverRunsGeneralAuthentication(test *testing.T) {
	service, storage, _, repo, request, _ := qualifiedMemoryFixturePG(test)
	if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	queries, err := service.PreparedMemoryQueries()
	if err != nil {
		test.Fatal(err)
	}
	for _, snapshotAuthority := range []bool{false, true} {
		test.Run(map[bool]string{false: "general-only", true: "snapshot-authority"}[snapshotAuthority], func(test *testing.T) {
			general, snapshots := 0, 0
			err := storage.WithinReadSnapshot(systemTestContext(), func(bound context.Context) error {
				current := inbound.WithDocumentReadAuthorizer(bound, func(current context.Context, _ domain.ContentHash) (context.Context, error) {
					general++
					return current, domain.ErrIntegrity
				})
				if snapshotAuthority {
					current = inbound.WithDocumentSnapshotReadAuthorizer(current, func(current context.Context, selected domain.ContentHash) (context.Context, error) {
						snapshots++
						if selected != repo || !storage.InReadOnlyTransaction(current) {
							test.Fatal("memory authority escaped borrowed read snapshot")
						}
						return current, nil
					})
				}
				page, err := queries.QueryPreparedMemory(current, repo, request)
				if snapshotAuthority {
					if !errors.Is(err, domain.ErrConflict) || snapshots != 0 {
						test.Fatal("minimum reader lost its owned-snapshot requirement", err, snapshots)
					}
				} else if !errors.Is(err, domain.ErrCatalogUnsupported) || snapshots != 0 {
					test.Fatal("prepared read accepted borrowed general-only authority", err, snapshots)
				}
				if general != 0 || page.Status.Generation != "" || len(page.Page.Items) != 0 {
					test.Fatal("general authentication ran in borrowed read or partial page escaped", general, page)
				}
				_, err = storage.GetRepo(bound, repo)
				return err
			})
			if err != nil {
				test.Fatal("prepared query poisoned the borrowed read", err)
			}
		})
	}
}

func TestPGPreparedMemoryUnknownTransactionWrapperPreservesBorrowedSnapshot(test *testing.T) {
	service, storage, _, repo, _, _ := qualifiedMemoryFixturePG(test)
	service.meta = struct {
		outbound.MetadataStore
		outbound.MinimumMemoryReadStore
		outbound.RepositoryTransactions
	}{storage, storage, storage}
	if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	queries, err := service.PreparedMemoryQueries()
	if !errors.Is(err, domain.ErrCatalogUnsupported) || queries != nil {
		test.Fatal("query assembly accepted an unidentifiable transaction capability", err)
	}
	general := 0
	scope := &preparedMemoryQueryScope{transactional: true}
	err = storage.WithinReadSnapshot(systemTestContext(), func(bound context.Context) error {
		current := inbound.WithDocumentReadAuthorizer(bound, func(current context.Context, _ domain.ContentHash) (context.Context, error) {
			general++
			return current, domain.ErrIntegrity
		})
		if err := scope.WithMemoryReadResources(current, repo, func(context.Context) error { test.Fatal("unknown transaction entered prepared read"); return nil }); !errors.Is(err, domain.ErrCatalogUnsupported) || general != 0 {
			test.Fatal("transaction wrapper ran general authority without presence proof", err, general)
		}
		if _, err := scope.authorizePreparation(current, repo); !errors.Is(err, domain.ErrCatalogUnsupported) || general != 0 {
			test.Fatal("unknown transaction entered preparation authority", err, general)
		}
		_, err := storage.GetRepo(bound, repo)
		return err
	})
	if err != nil {
		test.Fatal("unknown-presence query poisoned the borrowed read", err)
	}
}

func TestPGPreparedMemoryDemandRequiresFreshGeneralAuthority(test *testing.T) {
	for _, snapshotOnly := range []bool{false, true} {
		test.Run(map[bool]string{false: "general-revoked", true: "snapshot-only"}[snapshotOnly], func(test *testing.T) {
			service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
			memoryWorkConfigurePG(test, service, storage, repo)
			if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{repo}); err != nil {
				test.Fatal(err)
			}
			if err := service.ConfigurePreparedMemoryDemands([]domain.ContentHash{repo}); err != nil {
				test.Fatal(err)
			}
			queries, err := service.PreparedMemoryQueries()
			if err != nil {
				test.Fatal(err)
			}
			general, snapshots := 0, 0
			ctx := inbound.WithRepositoryActor(context.Background(), "reader")
			if !snapshotOnly {
				ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, selected domain.ContentHash) (context.Context, error) {
					general++
					actor, system := inbound.RepositoryActor(bound)
					if storage.InTransaction(bound) || selected != repo || actor != "reader" || system {
						test.Fatal("general preparation authority ran in a transaction or changed actor")
					}
					return bound, domain.ErrForbidden
				})
			}
			ctx = inbound.WithDocumentSnapshotReadAuthorizer(ctx, func(bound context.Context, selected domain.ContentHash) (context.Context, error) {
				snapshots++
				if !storage.InReadOnlyTransaction(bound) || selected != repo {
					test.Fatal("read authority escaped its snapshot")
				}
				return bound, nil
			})
			page, err := queries.QueryPreparedMemory(ctx, repo, request)
			expected, expectedGeneral := domain.ErrForbidden, 1
			if snapshotOnly {
				expected, expectedGeneral = domain.ErrUnauthorized, 0
			}
			if !errors.Is(err, expected) || general != expectedGeneral || snapshots != 2 || page.Status.Generation != "" {
				test.Fatal("read proof substituted for current preparation authority", err, general, snapshots, page)
			}
			var count int
			if err := connection.QueryRow(systemTestContext(), `SELECT count(*) FROM memory_read_work WHERE repo_id=$1`, repo).Scan(&count); err != nil || count != 0 {
				test.Fatal("unauthorized preparation enqueued work", count, err)
			}
		})
	}
}
