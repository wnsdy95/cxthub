package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docVisibleContractMeta struct {
	outbound.MetadataStore
}

func (*docVisibleContractMeta) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return domain.Repo{}, domain.ErrNotFound
}

func (*docVisibleContractMeta) GetSnapshot(_ context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	return domain.Snapshot{ID: hash, DocHash: hash, RepoID: repo, DocIdentity: domain.DocumentIdentityRootV1}, nil
}

func (*docVisibleContractMeta) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	return read(ctx)
}

func (*docVisibleContractMeta) WithinRepository(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
	return read(ctx)
}

type docVisibleContractStore struct {
	outbound.BlobStore
	outbound.DocRangeStore
	outbound.ConversationChunkReader
	ranges              domain.DocRangeGeneration
	visible             domain.DocVisibleGeneration
	binding             domain.DocReadGenerationBinding
	pageHashReads       int
	onVisibleDescriptor func()
}

func (*docVisibleContractStore) InReadOnlyTransaction(context.Context) bool {
	return true
}

func (*docVisibleContractStore) HasDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	return true, nil
}

func (*docVisibleContractStore) EnqueueDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	panic("unexpected preparation write")
}

func (storage *docVisibleContractStore) OpenDocRangeGeneration(context.Context, domain.ContentHash, domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	return &docVisibleContractRangeRead{storage: storage}, nil
}

func (storage *docVisibleContractStore) OpenDocVisibleGeneration(context.Context, domain.ContentHash, domain.ContentHash) (outbound.DocVisibleGenerationRead, error) {
	return &docVisibleContractRead{storage: storage}, nil
}

type docVisibleContractRangeRead struct {
	outbound.DocRangeGenerationRead
	storage *docVisibleContractStore
}

func (read *docVisibleContractRangeRead) Descriptor() domain.DocRangeGeneration {
	return read.storage.ranges
}

func (read *docVisibleContractRangeRead) Binding() domain.DocReadGenerationBinding {
	return read.storage.binding
}

func (read *docVisibleContractRangeRead) Envelope(ctx context.Context) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := read.Binding().Validate(); err != nil {
		return nil, err
	}
	return bytes.Clone(read.storage.ranges.Envelope), nil
}

func (read *docVisibleContractRangeRead) PageHash(context.Context, int) (domain.ContentHash, error) {
	read.storage.pageHashReads++
	return h('4'), nil
}

type docVisibleContractRead struct {
	outbound.DocVisibleGenerationRead
	storage *docVisibleContractStore
}

func (read *docVisibleContractRead) Descriptor() domain.DocVisibleGeneration {
	if read.storage.onVisibleDescriptor != nil {
		read.storage.onVisibleDescriptor()
	}
	return read.storage.visible
}

func (read *docVisibleContractRead) Binding() domain.DocReadGenerationBinding {
	return read.storage.binding
}

func newDocVisibleContractFixture(test *testing.T, total, envelopeBytes int) (*Service, *docVisibleContractStore) {
	test.Helper()
	envelope := historyEnvelope()
	envelope.Cwd = strings.Repeat("x", envelopeBytes)
	manifest, _, err := domain.ConversationManifestForCIR(domain.CIRDocument{Envelope: envelope, Events: []domain.CIREvent{}})
	if err != nil {
		test.Fatal(err)
	}
	ranges := domain.DocRangeGeneration{Version: 2, Algorithm: 1, Hash: h('2'), Envelope: manifest.Envelope, EventCount: int64(total), StreamBytes: int64(total) * 64}
	root := domain.DocVisibleRoot{}
	if total > 0 {
		ranges.EventPages = domain.DocRangeDirectoryRoot{Hash: h('4'), Count: (total + domain.DocRangePageEvents - 1) / domain.DocRangePageEvents}
		chunks := ranges.ChunkCount()
		ranges.ChunkPages = domain.DocRangeDirectoryRoot{Hash: h('5'), Count: (chunks + domain.DocRangeChunkPageSize - 1) / domain.DocRangeChunkPageSize}
		root = domain.DocVisibleRoot{Hash: h('6'), Units: total}
	}
	raw, err := ranges.Encode()
	if err != nil {
		test.Fatal("fixture range validation", err)
	}
	visible := domain.DocVisibleGeneration{
		Version: 1, Algorithm: domain.DocVisibleAlgorithm, Hash: h('2'), RangeGeneration: domain.HashContent(raw),
		Events: total, Rendered: total, Views: map[string]domain.DocVisibleRoot{"all": root, "chat": root, "prompts": root},
	}
	if _, err := visible.Encode(); err != nil {
		test.Fatal("fixture visible validation", err)
	}
	storage := &docVisibleContractStore{
		ranges: ranges, visible: visible,
		binding: domain.DocReadGenerationBinding{SourceID: "11111111-1111-4111-8111-111111111111", RangeGeneration: visible.RangeGeneration},
	}
	if err := storage.binding.Validate(); err != nil {
		test.Fatal(err)
	}
	return &Service{meta: &docVisibleContractMeta{}, blobs: storage}, storage
}

func TestDocVisibleContractTerminalPageBudget(test *testing.T) {
	for _, total := range []int{0, 1} {
		for _, envelopeBytes := range []int{0, docViewBytes + 4096} {
			test.Run(fmt.Sprintf("events=%d/envelope=%d", total, envelopeBytes), func(test *testing.T) {
				service, storage := newDocVisibleContractFixture(test, total, envelopeBytes)
				page, err := service.ReadDocVisiblePage(systemTestContext(), h('1'), h('2'), domain.DocVisiblePageRequest{View: "chat", Offset: total})
				if envelopeBytes > docViewBytes {
					empty := reflect.DeepEqual(page, domain.DocVisiblePage{})
					if !errors.Is(err, domain.ErrDocumentResourceLimit) || !empty {
						test.Fatalf("oversized terminal page: error=%v empty=%t; want resource limit and zero data", err, empty)
					}
				} else if err != nil || page.Hash != h('2') || page.Total != total || page.From != total || page.To != total || len(page.Units) != 0 || len(page.Events) != 0 || page.NextCursor != nil {
					test.Fatalf("valid terminal page: page=%+v error=%v", page, err)
				}
				if storage.pageHashReads != 0 {
					test.Fatal("terminal page traversed history", storage.pageHashReads)
				}
			})
		}
	}
}

func TestDocVisibleContractMissingBoundaryDoesNotTraverse(test *testing.T) {
	for _, total := range []int{128, 1280, 12800} {
		test.Run(fmt.Sprint(total), func(test *testing.T) {
			service, storage := newDocVisibleContractFixture(test, total, 0)
			page, err := service.ReadDocVisiblePage(systemTestContext(), h('1'), h('2'), domain.DocVisiblePageRequest{View: "chat", Base: h('2'), Offset: -1, Limit: 1})
			empty := reflect.DeepEqual(page, domain.DocVisiblePage{})
			if !errors.Is(err, domain.ErrDocVisibleProjectionUnsupported) || !empty || storage.pageHashReads != 0 {
				test.Fatalf("missing boundary capability: error=%v empty=%t page_hash_reads=%d; want unsupported, zero data and zero traversal", err, empty, storage.pageHashReads)
			}
		})
	}
}

func TestDocVisibleContractTerminalCancellationReturnsNoData(test *testing.T) {
	for _, total := range []int{0, 1} {
		test.Run(fmt.Sprint(total), func(test *testing.T) {
			service, storage := newDocVisibleContractFixture(test, total, 0)
			ctx, cancel := context.WithCancel(systemTestContext())
			defer cancel()
			storage.onVisibleDescriptor = cancel
			page, err := service.ReadDocVisiblePage(ctx, h('1'), h('2'), domain.DocVisiblePageRequest{View: "chat", Offset: total})
			empty := reflect.DeepEqual(page, domain.DocVisiblePage{})
			if !errors.Is(err, context.Canceled) || !empty {
				test.Fatalf("canceled terminal page: error=%v empty=%t; want cancellation and zero data", err, empty)
			}
		})
	}
}
