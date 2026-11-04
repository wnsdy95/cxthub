//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGContextBorrowedSnapshotNeverRunsGeneralAuthentication(test *testing.T) {
	service, storage, repo := collaborationPG(test)
	queries, err := service.ContextQueriesForProfile(inbound.ContextQueryPinnedSnapshots)
	if err != nil {
		test.Fatal(err)
	}
	for _, snapshotAuthority := range []bool{false, true} {
		test.Run(map[bool]string{false: "general-only", true: "snapshot-authority"}[snapshotAuthority], func(test *testing.T) {
			general, snapshots := 0, 0
			err := storage.WithinReadSnapshot(systemTestContext(), func(bound context.Context) error {
				request := inbound.WithDocumentReadAuthorizer(bound, func(current context.Context, _ domain.ContentHash) (context.Context, error) {
					general++
					return current, domain.ErrIntegrity
				})
				if snapshotAuthority {
					request = inbound.WithDocumentSnapshotReadAuthorizer(request, func(current context.Context, selected domain.ContentHash) (context.Context, error) {
						snapshots++
						if selected != repo || !storage.InReadOnlyTransaction(current) {
							test.Fatal("context authority escaped its borrowed read-only snapshot")
						}
						return current, nil
					})
				}
				view, err := queries.QueryContext(request, repo, domain.ContextSelection{})
				if snapshotAuthority {
					if err != nil || view.Version != 1 || snapshots != 1 {
						test.Fatal("context query did not use current snapshot-only authority", view, err, snapshots)
					}
				} else if !errors.Is(err, domain.ErrCatalogUnsupported) || view.Version != 0 || snapshots != 0 {
					test.Fatal("general-only authority was accepted inside a borrowed snapshot", view, err)
				}
				if general != 0 {
					test.Fatal("write-capable general authentication ran inside the borrowed snapshot", general)
				}
				_, err = storage.GetRepo(bound, repo)
				return err
			})
			if err != nil {
				test.Fatal("context authorization poisoned the borrowed read", err)
			}
		})
	}
}
