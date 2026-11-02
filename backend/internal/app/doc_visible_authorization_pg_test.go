//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGDocVisibleRevocationPreventsPreparationWrites(t *testing.T) {
	for _, boundary := range []bool{false, true} {
		fixture := newDocViewPGFixture(t)
		request := domain.DocVisiblePageRequest{View: "chat", Offset: -1}
		denyAt := 2
		if boundary {
			if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, fixture.hash); err != nil {
				t.Fatal(err)
			}
			previous := fixture.cir
			previous.Events = previous.Events[:123]
			request.Base = seedDocViewBoundaryPG(t, fixture, previous)
			denyAt = 6
		}
		calls := 0
		ctx := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
			calls++
			if repo != fixture.repo {
				return bound, domain.ErrIntegrity
			}
			if calls == denyAt {
				return bound, domain.ErrForbidden
			}
			return inbound.WithDocumentSnapshotReadAuthorizer(bound, func(pinned context.Context, requested domain.ContentHash) (context.Context, error) {
				if !fixture.storage.InReadOnlyTransaction(pinned) {
					t.Fatal("visible authority escaped the read-only snapshot")
				}
				if requested != fixture.repo || calls >= denyAt {
					return pinned, domain.ErrForbidden
				}
				return pinned, nil
			}), nil
		})
		page, err := fixture.service.ReadDocVisiblePage(ctx, fixture.repo, fixture.hash, request)
		if !errors.Is(err, domain.ErrForbidden) || page.Hash != "" || calls != denyAt {
			t.Fatal("revocation did not prevent enqueue", boundary, calls, err, page)
		}
		if _, err := fixture.storage.ClaimDocRangeJob(fixture.ctx, time.Now().UTC(), docJobLease); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("denied preparation enqueued work", boundary, err)
		}
	}
}
