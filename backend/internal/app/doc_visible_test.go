package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestDocVisibleRejectsInvalidSelectionBeforeStorage(t *testing.T) {
	selection := domain.DocVisibleSelector{Version: 1, Purpose: "units", Repo: h('1'), Hash: h('2'), Generation: h('3'), View: "chat", Total: 10, Unit: 2, Next: 2, Limit: 20}
	token, err := selection.Encode()
	if err != nil {
		t.Fatal(err)
	}
	service := newUnusedDocumentQueryService()
	for _, request := range []domain.DocVisiblePageRequest{
		{}, {View: "raw"}, {View: "chat", Offset: -2}, {View: "chat", Limit: 101}, {View: "chat", Base: "bad"},
		{Cursor: "bad"}, {Cursor: token, View: "chat"}, {Cursor: token, Base: h('4')}, {Cursor: token, Limit: 1},
		{Cursor: token, Expand: token}, {Expand: token}, {Cursor: token, Offset: 1},
	} {
		page, err := service.ReadDocVisiblePage(context.Background(), h('1'), h('2'), request)
		if !errors.Is(err, domain.ErrValidation) || page.Hash != "" {
			t.Fatal(request, page, err)
		}
	}
	for _, pair := range [][2]domain.ContentHash{{h('4'), h('2')}, {h('1'), h('4')}, {"invalid", h('2')}} {
		if _, err := service.ReadDocVisiblePage(context.Background(), pair[0], pair[1], domain.DocVisiblePageRequest{Cursor: token}); !errors.Is(err, domain.ErrValidation) {
			t.Fatal(err)
		}
	}
}

type visibleBoundaryAuthorizationStore struct {
	outbound.BlobStore
	outbound.DocRangeBoundaryStore
}

func TestDocVisibleReauthorizesAfterAdmissionBeforeReadsAndBoundaryJobs(t *testing.T) {
	selection := domain.DocVisibleSelector{Version: 1, Purpose: "units", Repo: h('1'), Hash: h('2'), Generation: h('3'), View: "chat", Total: 1, Limit: 1}
	token, err := selection.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"prepare", "continuation", "boundary"} {
		t.Run(phase, func(t *testing.T) {
			ctx, gate := admissionTestContext(time.Second)
			var calls atomic.Int32
			ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
				calls.Add(1)
				if repo != h('1') {
					return bound, domain.ErrIntegrity
				}
				return bound, domain.ErrForbidden
			})
			_, releaseBulk, err := AcquireResources(ctx, ResourceBulk)
			if err != nil {
				t.Fatal(err)
			}
			defer releaseBulk()
			_, releaseRead, err := AcquireResources(ctx, ResourceInteractive)
			if err != nil {
				t.Fatal(err)
			}
			defer releaseRead()
			done := make(chan error, 1)
			go func() {
				service := &Service{meta: &documentQueryUnusedStore{}, blobs: &visibleBoundaryAuthorizationStore{}}
				var err error
				switch phase {
				case "prepare":
					_, err = service.ReadDocVisiblePage(ctx, h('1'), h('2'), domain.DocVisiblePageRequest{View: "chat", Offset: -1})
				case "continuation":
					_, err = service.ReadDocVisiblePage(ctx, h('1'), h('2'), domain.DocVisiblePageRequest{Cursor: token})
				case "boundary":
					err = service.prepareDocViewBoundary(ctx, h('1'), h('2'), h('3'))
				}
				done <- err
			}()
			waitResourceQueue(t, gate, ResourceInteractive, 1)
			if calls.Load() != 0 {
				t.Fatal("authorization ran before queued admission")
			}
			releaseRead()
			if err := <-done; !errors.Is(err, domain.ErrForbidden) || calls.Load() != 1 {
				t.Fatal("post-wait revocation ignored", err, calls.Load())
			}
			releaseBulk()
			assertResourcesReleased(t, gate)
		})
	}
	if _, err := newUnusedDocumentQueryService().ReadDocVisiblePage(context.Background(), h('1'), h('2'), domain.DocVisiblePageRequest{View: "chat", Offset: -1}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("unauthorized reader reached storage", err)
	}
}

func TestDocVisibleUnsupportedStoreDoesNotMaterializeOrMutate(t *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
	storage := store.NewFSStore(dir)
	doc, _ := seedRootReadDoc(t, dir, storage, repo, historyMessage(domain.RoleUser, "unsupported fixture"))
	observed := &docViewCountStore{FSStore: storage}
	service := NewService(storage, observed, nil, nil, nil)
	page, err := service.ReadDocVisiblePage(ctx, repo, doc.Hash, domain.DocVisiblePageRequest{View: "chat", Offset: -1})
	if !errors.Is(err, domain.ErrDocVisibleProjectionUnsupported) || page.Hash != "" || observed.fullReads != 0 || observed.chunkReads != 0 || observed.pageReads != 0 {
		t.Fatal("unsupported read performed legacy work", page, err, observed)
	}
}

func TestDocVisibleSelectionGenerationBindsBothSourceLifetimes(t *testing.T) {
	generation := domain.DocVisibleGeneration{Version: 1, Algorithm: domain.DocVisibleAlgorithm, Hash: h('2'), RangeGeneration: h('3'), Views: map[string]domain.DocVisibleRoot{"all": {}, "chat": {}, "prompts": {}}}
	source := domain.DocReadGenerationBinding{SourceID: "4b94be7c-fae7-4c49-8ee5-bd944755d1ce", RangeGeneration: h('3')}
	previous := domain.DocReadGenerationBinding{SourceID: "4b94be7c-fae7-4c49-8ee5-bd944755d1cf", RangeGeneration: h('4')}
	baseline, err := visibleSelectionGeneration(generation, source, h('5'), previous)
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []bool{false, true} {
		for _, incarnation := range []bool{false, true} {
			doc, parent := source, previous
			binding := &doc
			if base {
				binding = &parent
			}
			if incarnation {
				binding.SourceID = "4b94be7c-fae7-4c49-8ee5-bd944755d1cd"
			} else {
				binding.SourceVersion++
			}
			changed, err := visibleSelectionGeneration(generation, doc, h('5'), parent)
			if err != nil || changed == baseline {
				t.Fatal("generation reused a changed lifetime", base, incarnation, err)
			}
		}
	}
	for _, binding := range []domain.DocReadGenerationBinding{{}, {SourceID: source.SourceID, RangeGeneration: h('6')}} {
		if _, err := visibleSelectionGeneration(generation, binding, "", domain.DocReadGenerationBinding{}); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("invalid binding accepted", err)
		}
	}
}
