//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGContextSegmentsRejectChangedOriginButNotUnrelatedPending(test *testing.T) {
	service, storage, original := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	metadata := domain.Repo{ID: hh(string(original) + "-origin"), DefaultBranch: "main"}
	if _, err := storage.PutRepo(ctx, metadata); err != nil {
		test.Fatal(err)
	}
	first := collaborationSnapshot(test, storage, metadata.ID, "segment-first")
	second := collaborationSnapshot(test, storage, metadata.ID, "segment-second", first)
	request := domain.ContextSelection{Scope: "all", SegmentLimit: 1}
	before, err := service.QueryContext(ctx, metadata.ID, request)
	if err != nil || before.Segments == nil || before.Segments.Next != 1 {
		test.Fatal("fixture needs two segment pages", err)
	}
	if err := storage.PutPending(ctx, metadata.ID, domain.Pending{RepoID: metadata.ID, SessionID: "unrelated-live", Target: second, UpdatedAt: time.Now().UTC()}); err != nil {
		test.Fatal(err)
	}
	request.SegmentOffset, request.SegmentStateHash = before.Segments.Next, before.StateHash
	continued, err := service.QueryContext(ctx, metadata.ID, request)
	if err != nil || continued.StateHash != before.StateHash || continued.DeliveryStateHash != before.DeliveryStateHash {
		test.Fatal("unrelated pending changed selected segment continuation", err)
	}
	metadata.GitRemoteURL = "https://github.com/example/newly-bound-segments"
	err = storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		held, err := service.QueryContext(bound, metadata.ID, request)
		if err != nil {
			return err
		}
		if _, err := storage.PutRepo(ctx, metadata); err != nil {
			return err
		}
		again, err := service.QueryContext(bound, metadata.ID, request)
		if err == nil && !reflect.DeepEqual(again, held) {
			test.Fatal("pinned continuation mixed origin generations")
		}
		return err
	})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := service.QueryContext(ctx, metadata.ID, request); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("old segment continuation survived current origin change", err)
	}
	request.SegmentOffset, request.SegmentStateHash = 0, ""
	after, err := service.QueryContext(ctx, metadata.ID, request)
	if err != nil || after.StateHash == before.StateHash || after.DeliveryStateHash == before.DeliveryStateHash {
		test.Fatal("new selection did not bind current origin", err)
	}
}
