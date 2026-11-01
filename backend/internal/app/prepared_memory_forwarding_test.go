package app

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestRequestPreparedMemoryRequiresUnpinnedFloorAndBothScopes(test *testing.T) {
	for _, scenario := range []string{"accepted", "no_cursor", "pinned", "read_disabled", "demand_disabled", "wrong_target"} {
		test.Run(scenario, func(test *testing.T) {
			target, cursor := preparedCursorFixture(test)
			probe := &preparedMemoryDemandProbe{pendingPreparedMemoryStore: pendingPreparedMemoryStore{requirement: cursor.Requirement}}
			service := &Service{meta: probe}
			if scenario != "read_disabled" {
				if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{target.RepoID}); err != nil {
					test.Fatal(err)
				}
			}
			if scenario != "demand_disabled" {
				if err := service.ConfigurePreparedMemoryDemands([]domain.ContentHash{target.RepoID}); err != nil {
					test.Fatal(err)
				}
			}
			if scenario != "pinned" {
				cursor.Generation = ""
			}
			if scenario == "wrong_target" {
				cursor.Requirement.TargetID = domain.HashContent([]byte("foreign"))
			}
			encoded, err := encodePreparedMemoryCursor(cursor)
			if err != nil {
				test.Fatal(err)
			}
			if scenario == "no_cursor" {
				encoded = ""
			}
			err = service.RequestPreparedMemory(systemTestContext(), target.RepoID, domain.EffectiveMemoryRequest{Selection: target.Selection, Content: target.Content, Cursor: encoded})
			if scenario == "accepted" {
				if err != nil || len(probe.requested) != 1 || probe.requested[0] != cursor.Requirement {
					test.Fatal("forwarded demand changed floor", err)
				}
			} else {
				expected := domain.ErrValidation
				if scenario == "read_disabled" || scenario == "demand_disabled" {
					expected = domain.ErrCatalogUnsupported
				}
				if !errors.Is(err, expected) || len(probe.requested) != 0 {
					test.Fatal("invalid forwarding registered work", err)
				}
			}
		})
	}
}
