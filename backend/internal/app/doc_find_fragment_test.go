package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestDocFindFragmentLegacyReconstructsBoundedUTF8AndPrevious(test *testing.T) {
	ctx, repo, directory := systemTestContext(), h('1'), test.TempDir()
	storage := store.NewFSStore(directory)
	doc := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{
		historyMessage(domain.RoleUser, "untouched prefix"),
		{Kind: domain.EventToolResult, Output: strings.Repeat("한🙂éx", 80_000)},
	})
	verified, err := domain.VerifySessionDoc(doc)
	if err != nil {
		test.Fatal(err)
	}
	index, err := storage.DocReadIndex(ctx, repo, doc.Hash)
	if err != nil {
		test.Fatal(err)
	}
	entry := index.Events[1]
	want, err := verified.EventStreamRange(ctx, entry.Offset, entry.Length)
	if err != nil {
		test.Fatal(err)
	}
	before := legacyCompatibilityFiles(test, directory)
	observed := &countedReadStore{FSStore: storage}
	service := NewService(storage, observed, nil, nil, nil)
	request := domain.DocFindFragmentRequest{Legacy: true, Index: 1, Total: 2, Budget: 65536}
	var reconstructed []byte
	for {
		chunkReads := observed.chunks
		page, err := service.ReadDocFindFragment(ctx, repo, doc.Hash, request)
		if err != nil || page.Projection != domain.DocLegacyFindFragmentProjection || page.Generation != "" || page.Hash != doc.Hash || page.Index != 1 || page.Offset != request.Offset || page.Total != len(want) || len(page.JSON) == 0 || len(page.JSON) > request.Budget || !utf8.ValidString(page.JSON) {
			test.Fatal("invalid fragment", page, err)
		}
		if observed.whole != 0 || observed.chunks-chunkReads > 4 {
			test.Fatal("fragment materialized an event or archive", observed.whole, observed.chunks-chunkReads)
		}
		if page.Offset == 0 {
			if page.Previous != -1 {
				test.Fatal("first fragment has a previous page", page.Previous)
			}
		} else {
			if page.Previous < max(0, page.Offset-request.Budget-3) || page.Previous >= page.Offset || !utf8.RuneStart(want[page.Previous]) {
				test.Fatal("unsafe backward byte pointer", page.Previous, page.Offset)
			}
			backward := request
			backward.Offset = page.Previous
			previous, err := service.ReadDocFindFragment(ctx, repo, doc.Hash, backward)
			if err != nil || !bytes.Equal([]byte(previous.JSON), want[backward.Offset:backward.Offset+len(previous.JSON)]) {
				test.Fatal("previous fragment changed source bytes", previous, err)
			}
		}
		reconstructed = append(reconstructed, page.JSON...)
		if page.Next == -1 {
			if page.Offset+len(page.JSON) != len(want) {
				test.Fatal("premature EOF", page)
			}
			break
		}
		if page.Next != page.Offset+len(page.JSON) || page.Next >= len(want) {
			test.Fatal("invalid forward byte pointer", page)
		}
		request.Offset = page.Next
	}
	if !bytes.Equal(reconstructed, want) || !reflect.DeepEqual(legacyCompatibilityFiles(test, directory), before) {
		test.Fatal("fragment bytes or persistent storage changed")
	}
}

func TestDocFindFragmentLegacyChecksActualTotalBoundaryAndFreshAuthority(test *testing.T) {
	ctx, repo := systemTestContext(), h('1')
	storage := store.NewFSStore(test.TempDir())
	doc := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{{Kind: domain.EventToolResult, Output: "한🙂needle"}})
	service := NewService(storage, storage, nil, nil, nil)
	request := domain.DocFindFragmentRequest{Legacy: true, Total: 1, Budget: 4}
	first, err := service.ReadDocFindFragment(ctx, repo, doc.Hash, request)
	if err != nil {
		test.Fatal(err)
	}
	index, err := storage.DocReadIndex(ctx, repo, doc.Hash)
	if err != nil {
		test.Fatal(err)
	}
	verified, err := domain.VerifySessionDoc(doc)
	if err != nil {
		test.Fatal(err)
	}
	raw, err := verified.EventStreamRange(ctx, index.Events[0].Offset, index.Events[0].Length)
	if err != nil {
		test.Fatal(err)
	}
	for _, offset := range []int{len(raw), len(raw) + 1, bytes.Index(raw, []byte("한")) + 1} {
		invalid := request
		invalid.Offset = offset
		page, err := service.ReadDocFindFragment(ctx, repo, doc.Hash, invalid)
		if !errors.Is(err, domain.ErrValidation) || page != (domain.DocFindFragmentPage{}) {
			test.Fatal("non-boundary offset accepted", page, err)
		}
	}
	invalid := request
	invalid.Total++
	if page, err := service.ReadDocFindFragment(ctx, repo, doc.Hash, invalid); !errors.Is(err, domain.ErrIntegrity) || page != (domain.DocFindFragmentPage{}) {
		test.Fatal("declared total was trusted", page, err)
	}
	allowed, calls := true, 0
	authorized := inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
		calls++
		if requested != repo || !allowed {
			return bound, domain.ErrForbidden
		}
		return bound, nil
	})
	if _, err := service.ReadDocFindFragment(authorized, repo, doc.Hash, request); err != nil || calls != 1 {
		test.Fatal("initial fresh permission failed", err, calls)
	}
	allowed, request.Offset = false, first.Next
	if page, err := service.ReadDocFindFragment(authorized, repo, doc.Hash, request); !errors.Is(err, domain.ErrForbidden) || calls != 2 || page != (domain.DocFindFragmentPage{}) {
		test.Fatal("continuation reused old permission", page, err, calls)
	}
}

func TestDocFindFragmentLegacyCompatibilityDoesNotWrite(test *testing.T) {
	for _, mode := range []string{"cold_chunked", "cold_monolithic", "indexed_monolithic"} {
		test.Run(mode, func(test *testing.T) {
			ctx, repo, directory := systemTestContext(), h('1'), test.TempDir()
			storage := store.NewFSStore(directory)
			doc := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{historyMessage(domain.RoleUser, "한🙂needle")})
			if mode != "indexed_monolithic" {
				legacyCompatibilityRemoveIndex(test, directory, repo, doc.Hash)
			}
			if mode != "cold_chunked" {
				legacyCompatibilityMonolithic(test, directory, repo, doc)
			}
			before := legacyCompatibilityFiles(test, directory)
			for _, readOnly := range []bool{false, true} {
				read := ctx
				if readOnly {
					read = outbound.WithDocReadOnly(read)
				}
				page, err := NewService(storage, storage, nil, nil, nil).ReadDocFindFragment(read, repo, doc.Hash,
					domain.DocFindFragmentRequest{Legacy: true, Total: 1, Budget: 65536})
				if err != nil || page.Next != -1 || page.Previous != -1 || !strings.Contains(page.JSON, "한🙂needle") || page.Generation != "" {
					test.Fatal("old legacy source rejected", page, err)
				}
			}
			if !reflect.DeepEqual(legacyCompatibilityFiles(test, directory), before) {
				test.Fatal("legacy compatibility read wrote persistent state")
			}
		})
	}
}

func TestDocFindFragmentOwnershipAndCorruptionFailClosedEveryRead(test *testing.T) {
	for _, scenario := range []string{"foreign", "removed_owner", "corrupt_chunk", "corrupt_index"} {
		test.Run(scenario, func(test *testing.T) {
			ctx, repo, directory := systemTestContext(), h('1'), test.TempDir()
			storage := store.NewFSStore(directory)
			doc := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{{Kind: domain.EventToolResult, Output: "needle 한🙂"}})
			service := NewService(storage, storage, nil, nil, nil)
			request := domain.DocFindFragmentRequest{Legacy: true, Total: 1, Budget: 4}
			first, err := service.ReadDocFindFragment(ctx, repo, doc.Hash, request)
			if err != nil {
				test.Fatal(err)
			}
			request.Offset = first.Next
			expected := domain.ErrIntegrity
			switch scenario {
			case "foreign":
				repo, expected = h('2'), domain.ErrNotFound
			case "removed_owner":
				if err := os.Remove(rootReadObjectPath(directory, repo, "docs", doc.Hash)); err != nil {
					test.Fatal(err)
				}
				expected = domain.ErrNotFound
			case "corrupt_chunk":
				manifest, err := storage.GetDocManifest(ctx, repo, doc.Hash)
				if err != nil {
					test.Fatal(err)
				}
				legacyCompatibilityWrite(test, rootReadObjectPath(directory, repo, "chunks", manifest.Chunks[0]), []byte("tampered bytes"))
			case "corrupt_index":
				legacyCompatibilityWrite(test, legacyCompatibilityIndexPath(directory, repo, doc.Hash), []byte("not an index"))
			}
			before := legacyCompatibilityFiles(test, directory)
			page, err := service.ReadDocFindFragment(ctx, repo, doc.Hash, request)
			if !errors.Is(err, expected) || page != (domain.DocFindFragmentPage{}) {
				test.Fatal("failed source returned fragment bytes", page, err)
			}
			if !reflect.DeepEqual(legacyCompatibilityFiles(test, directory), before) {
				test.Fatal("failed fragment modified persistent storage")
			}
		})
	}
}

func TestDocFindFragmentRootNeverDowngradesOrMaterializes(test *testing.T) {
	for _, mode := range []string{"root_snapshot", "missing_snapshot", "legacy_snapshot"} {
		test.Run(mode, func(test *testing.T) {
			ctx, repo, directory := systemTestContext(), h('1'), test.TempDir()
			storage := store.NewFSStore(directory)
			var identity *domain.DocumentIdentity
			if mode != "missing_snapshot" {
				value := domain.DocumentIdentityLegacy
				if mode == "root_snapshot" {
					value = domain.DocumentIdentityRootV1
				}
				identity = &value
			}
			doc, _ := seedRootReadDocWithIdentity(test, directory, storage, repo, identity, historyMessage(domain.RoleUser, "needle"))
			observed := &countedReadStore{FSStore: storage}
			service := NewService(storage, observed, nil, nil, nil)
			selection := domain.DocVisibleSelector{Version: 1, Purpose: "units", Repo: repo, Hash: doc.Hash, Generation: h('3'), View: "all", Total: 1, Limit: 1}
			token, err := selection.Encode()
			if err != nil {
				test.Fatal(err)
			}
			before := legacyCompatibilityFiles(test, directory)
			for _, request := range []domain.DocFindFragmentRequest{{Legacy: true, Total: 1, Budget: 4}, {Selection: token, Budget: 4}} {
				page, err := service.ReadDocFindFragment(ctx, repo, doc.Hash, request)
				if err == nil || page != (domain.DocFindFragmentPage{}) || observed.whole != 0 || observed.chunks != 0 {
					test.Fatal("Root fragment fell back to whole legacy reads", page, err, observed)
				}
			}
			if !reflect.DeepEqual(legacyCompatibilityFiles(test, directory), before) {
				test.Fatal("unsupported Root fragment wrote persistent state")
			}
		})
	}
}

type findFragmentUnindexedStore struct {
	outbound.BlobStore
	doc   domain.SessionDoc
	reads int
}

func (storage *findFragmentUnindexedStore) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	storage.reads++
	return storage.doc, nil
}

func TestDocFindFragmentUnindexedCompatibilityEnforcesActualDocumentRef(test *testing.T) {
	ctx, repo := systemTestContext(), h('1')
	storage := store.NewFSStore(test.TempDir())
	doc := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{historyMessage(domain.RoleUser, "needle")})
	for _, mode := range []string{"legacy", "root", "wrong_hash"} {
		candidate := doc
		if mode == "root" {
			candidate.Identity = domain.DocumentIdentityRootV1
		} else if mode == "wrong_hash" {
			candidate.Hash = h('3')
		}
		observed := &findFragmentUnindexedStore{BlobStore: storage, doc: candidate}
		page, err := NewService(storage, observed, nil, nil, nil).ReadDocFindFragment(ctx, repo, doc.Hash,
			domain.DocFindFragmentRequest{Legacy: true, Total: 1, Budget: 65536})
		if mode == "legacy" {
			if err != nil || !strings.Contains(page.JSON, "needle") || observed.reads != 1 {
				test.Fatal("unindexed legacy source rejected", page, err)
			}
		} else if !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) || page != (domain.DocFindFragmentPage{}) || observed.reads != 1 {
			test.Fatal("unindexed source identity was relabeled", page, err)
		}
	}
}

func TestDocFindFragmentReauthorizesAfterInteractiveAdmission(test *testing.T) {
	selection := domain.DocVisibleSelector{Version: 1, Purpose: "units", Repo: h('1'), Hash: h('2'), Generation: h('3'), View: "all", Total: 1, Limit: 1}
	token, err := selection.Encode()
	if err != nil {
		test.Fatal(err)
	}
	for _, root := range []bool{false, true} {
		ctx, gate := admissionTestContext(time.Second)
		var calls atomic.Int32
		ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
			calls.Add(1)
			return bound, domain.ErrForbidden
		})
		_, releaseBulk, err := AcquireResources(ctx, ResourceBulk)
		if err != nil {
			test.Fatal(err)
		}
		_, releaseRead, err := AcquireResources(ctx, ResourceInteractive)
		if err != nil {
			test.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			service := newUnusedDocumentQueryService()
			var err error
			if root {
				_, err = service.ReadDocFindFragment(ctx, h('1'), h('2'), domain.DocFindFragmentRequest{Selection: token, Budget: 4})
			} else {
				_, err = service.ReadDocFindFragment(ctx, h('1'), h('2'), domain.DocFindFragmentRequest{Legacy: true, Total: 1, Budget: 4})
			}
			done <- err
		}()
		waitResourceQueue(test, gate, ResourceInteractive, 1)
		if calls.Load() != 0 {
			test.Fatal("authorization ran before interactive admission")
		}
		releaseRead()
		if err := <-done; !errors.Is(err, domain.ErrForbidden) || calls.Load() != 1 {
			test.Fatal("admitted fragment ignored permission revocation", err, calls.Load())
		}
		releaseBulk()
		assertResourcesReleased(test, gate)
	}
}

func TestDocFindFragmentRejectsInvalidOrUnauthorizedReadsBeforeStorage(test *testing.T) {
	service := newUnusedDocumentQueryService()
	for _, request := range []domain.DocFindFragmentRequest{
		{}, {Selection: "invalid", Budget: 4}, {Legacy: true, Total: 1, Budget: 3},
		{Legacy: true, Total: 1, Index: 1, Budget: 4}, {Legacy: true, Total: 1, Budget: 4, Offset: -1},
		{Legacy: true, Selection: "mixed", Total: 1, Budget: 4},
	} {
		page, err := service.ReadDocFindFragment(context.Background(), h('1'), h('2'), request)
		if !errors.Is(err, domain.ErrValidation) || page != (domain.DocFindFragmentPage{}) {
			test.Fatal("invalid request reached storage", request, page, err)
		}
	}
	request := domain.DocFindFragmentRequest{Legacy: true, Total: 1, Budget: 4}
	if page, err := service.ReadDocFindFragment(context.Background(), h('1'), h('2'), request); !errors.Is(err, domain.ErrUnauthorized) || page != (domain.DocFindFragmentPage{}) {
		test.Fatal("unauthorized request reached storage", page, err)
	}
	canceled, cancel := context.WithCancel(systemTestContext())
	cancel()
	if page, err := service.ReadDocFindFragment(canceled, h('1'), h('2'), request); !errors.Is(err, context.Canceled) || page != (domain.DocFindFragmentPage{}) {
		test.Fatal("canceled request reached storage", page, err)
	}
}
