package app

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestContextSegmentCursorRejectsOriginBinding(test *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "direct"
		if cached {
			name = "cached"
		}
		test.Run(name, func(test *testing.T) {
			service, storage := newFsckSvc(test)
			ctx := systemTestContext()
			repo := domain.Repo{ID: hh(test.Name())}
			if _, err := storage.PutRepo(ctx, repo); err != nil {
				test.Fatal(err)
			}
			first := segmentCapture(test, storage, repo.ID, []string{"first"})
			segmentCapture(test, storage, repo.ID, []string{"first", "second"}, first.ID)
			source := &effectiveMemorySourceStore{FSStore: storage, source: testProjectionGeneration(repo.ID), readOnly: true}
			if cached {
				service.meta = source
			}
			request := domain.ContextSelection{Scope: "all", SegmentLimit: 1}
			before, err := service.QueryContext(ctx, repo.ID, request)
			if err != nil || before.Segments == nil || before.Segments.Next != 1 {
				test.Fatal("fixture needs two context segments", err)
			}
			repo.GitRemoteURL = "https://github.com/example/new-binding"
			if _, err := storage.PutRepo(ctx, repo); err != nil {
				test.Fatal(err)
			}
			source.source.Sequence++
			after, err := service.QueryContext(ctx, repo.ID, request)
			if err != nil || after.DeliveryStateHash == before.DeliveryStateHash {
				test.Fatal("fixture did not change selected origin", err)
			}
			request.SegmentOffset, request.SegmentStateHash = before.Segments.Next, before.StateHash
			if _, err := service.QueryContext(ctx, repo.ID, request); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("cursor joined segments from different origin bindings", err)
			}
		})
	}
}
