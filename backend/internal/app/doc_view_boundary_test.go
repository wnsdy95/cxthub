package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docViewBoundaryFake struct {
	*docViewCountStore
	readonly, ready bool
	boundary        int
	failure         error
	queued, reads   int
	state, reason   string
}

func (storage *docViewBoundaryFake) InReadOnlyTransaction(context.Context) bool {
	return storage.readonly
}

func (storage *docViewBoundaryFake) DocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash) (int, error) {
	storage.reads++
	if storage.failure != nil {
		return 0, storage.failure
	}
	if !storage.ready {
		return 0, domain.ErrDocRangeIndexUnavailable
	}
	return storage.boundary, nil
}

func (storage *docViewBoundaryFake) EnqueueDocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	storage.queued++
	return domain.DocRangeIndexJob{State: storage.state, Reason: storage.reason}, nil
}

func TestDocViewBoundaryNeverFallsBackAfterGenerationFailure(t *testing.T) {
	ctx := context.Background()
	for _, readonly := range []bool{true, false} {
		for _, failure := range []error{domain.ErrIntegrity, domain.ErrNotFound, domain.ErrDocRangeIndexUnavailable, context.Canceled} {
			storage := &docViewBoundaryFake{readonly: readonly, failure: failure}
			service := NewService(nil, storage, nil, nil, nil)
			source := docViewSource{root: true, total: 128, block: func(int) (domain.ContentHash, error) {
				t.Fatal("persisted boundary failure fell back to prefix traversal")
				return "", nil
			}}
			_, err := service.docViewBoundary(ctx, h('1'), h('2'), h('3'), source, source)
			if readonly {
				if !errors.Is(err, failure) || storage.reads != 1 {
					t.Fatal("boundary failure changed", err, storage.reads)
				}
			} else if !errors.Is(err, domain.ErrConflict) || storage.reads != 0 {
				t.Fatal("unbound boundary lookup", err, storage.reads)
			}
		}
	}
	for _, boundary := range []int{-1, 129} {
		storage := &docViewBoundaryFake{readonly: true, ready: true, boundary: boundary}
		service := NewService(nil, storage, nil, nil, nil)
		if _, err := service.docViewBoundary(ctx, h('1'), h('2'), h('3'), docViewSource{root: true, total: 128}, docViewSource{root: true, total: 200}); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("invalid persisted boundary accepted", boundary, err)
		}
	}
}

func TestDocViewBoundaryPreparationQueuesWithoutReadingEvents(t *testing.T) {
	ctx, repo, directory := systemTestContext(), h('1'), t.TempDir()
	storage := store.NewFSStore(directory)
	base, _ := seedRootReadDoc(t, directory, storage, repo, historyMessage(domain.RoleUser, "shared prompt"))
	doc, _ := seedRootReadDoc(t, directory, storage, repo, historyMessage(domain.RoleUser, "shared prompt"), historyMessage(domain.RoleAssistant, "new response"))
	for _, hash := range []domain.ContentHash{base.Hash, doc.Hash} {
		if err := storage.EnsureDocRangeIndex(ctx, repo, hash); err != nil {
			t.Fatal(err)
		}
	}
	fake := &docViewBoundaryFake{docViewCountStore: &docViewCountStore{FSStore: storage}, readonly: true, state: "waiting", boundary: 1}
	service := NewService(storage, fake, nil, nil, nil)
	if _, err := service.ReadDocViewEvents(ctx, repo, doc.Hash, base.Hash, -1, 20, "chat"); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		t.Fatal("missing boundary computed on request", err)
	}
	if fake.queued != 1 || fake.pageReads != 0 || fake.chunkReads != 0 || fake.fullReads != 0 {
		t.Fatal("foreground traversed data", fake.queued, fake.pageReads, fake.chunkReads, fake.fullReads)
	}
	if _, err := service.ReadDocViewEvents(outbound.WithDocReadOnly(ctx), repo, doc.Hash, base.Hash, -1, 20, "chat"); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || fake.queued != 1 {
		t.Fatal("read-only caller enqueued", err, fake.queued)
	}
	fake.state = "completed"
	beforeReads := fake.reads
	if page, err := service.ReadDocViewEvents(ctx, repo, doc.Hash, base.Hash, -1, 20, "chat"); !errors.Is(err, domain.ErrDocRangeIndexPending) || page.Hash != "" || fake.reads-beforeReads != 2 || fake.queued != 3 {
		t.Fatal("repeated invalidation escaped bounded preparation", page, err, fake.reads-beforeReads, fake.queued)
	}
	if fake.pageReads != 0 || fake.chunkReads != 0 || fake.fullReads != 0 {
		t.Fatal("repeated invalidation replayed document", fake.pageReads, fake.chunkReads, fake.fullReads)
	}
	fake.ready = true
	page, err := service.ReadDocViewEvents(ctx, repo, doc.Hash, base.Hash, -1, 20, "chat")
	if err != nil || page.Offset != 1 || page.Inherited != 1 || len(page.Events) != 1 || page.Events[0].Index != 1 || fake.pageReads != 1 {
		t.Fatal("ready boundary failed or scanned prefix", page, fake.pageReads, err)
	}
	for _, failure := range []error{domain.ErrIntegrity, domain.ErrNotFound, context.Canceled} {
		fake.failure = failure
		if _, err := service.ReadDocViewEvents(ctx, repo, doc.Hash, base.Hash, -1, 20, "chat"); !errors.Is(err, failure) || fake.queued != 3 {
			t.Fatal("terminal failure queued repair", err, fake.queued)
		}
	}
}
