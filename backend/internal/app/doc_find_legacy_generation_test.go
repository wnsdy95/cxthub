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

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyFindGenerationCosts struct {
	opens, pages, directories, metadataBytes     int
	manifests, manifestBytes, chunks, chunkBytes int
	indexes, whole, boundaries, writes           int
	chunkLookups                                 int
}

type legacyFindGenerationFixture struct {
	generation domain.DocRangeGeneration
	binding    domain.DocReadGenerationBinding
	objects    map[domain.ContentHash][]byte
	owned      bool
	failure    error
}

type legacyFindGenerationStore struct {
	*store.FSStore
	repo     domain.ContentHash
	fixtures map[domain.ContentHash]*legacyFindGenerationFixture
	readonly bool
	prefix   int
	cost     legacyFindGenerationCosts
}

type legacyFindGenerationRead struct {
	storage *legacyFindGenerationStore
	fixture *legacyFindGenerationFixture
}

type legacyFindGenerationMeta struct {
	*store.FSStore
	snapshot domain.Snapshot
	failure  error
}

type legacyFindGenerationMalformedStore struct {
	*legacyFindGenerationStore
	mutation string
}

type legacyFindGenerationMalformedRead struct {
	outbound.DocRangeGenerationRead
	storage *legacyFindGenerationMalformedStore
}

func (storage *legacyFindGenerationMalformedStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	read, err := storage.legacyFindGenerationStore.OpenDocRangeGeneration(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	return &legacyFindGenerationMalformedRead{DocRangeGenerationRead: read, storage: storage}, nil
}

func (read *legacyFindGenerationMalformedRead) ReadPage(ctx context.Context, position int) ([]domain.DocRangeEntry, error) {
	entries, err := read.DocRangeGenerationRead.ReadPage(ctx, position)
	if err != nil {
		return nil, err
	}
	switch read.storage.mutation {
	case "short page":
		return entries[:len(entries)-1], nil
	case "empty page":
		return nil, nil
	case "wrong position":
		entries[len(entries)-1].Index++
	case "read failure":
		return nil, domain.ErrIntegrity
	}
	return entries, nil
}

func (metadata *legacyFindGenerationMeta) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	return metadata.snapshot, metadata.failure
}

func newLegacyFindGenerationStore(test *testing.T) *legacyFindGenerationStore {
	test.Helper()
	return &legacyFindGenerationStore{FSStore: store.NewFSStore(test.TempDir()), repo: h('1'), readonly: true,
		fixtures: map[domain.ContentHash]*legacyFindGenerationFixture{}}
}

func (storage *legacyFindGenerationStore) seed(test *testing.T, events []domain.CIREvent) domain.SessionDoc {
	test.Helper()
	doc := seedLegacyFindDocument(test, storage.FSStore, storage.repo, events)
	if err := storage.PutSnapshot(systemTestContext(), domain.Snapshot{ID: doc.Hash, RepoID: storage.repo, DocHash: doc.Hash}); err != nil {
		test.Fatal(err)
	}
	verified, err := domain.VerifySessionDoc(doc)
	if err != nil {
		test.Fatal(err)
	}
	generation, objects, err := domain.BuildLegacyDocRangeGeneration(systemTestContext(), verified)
	if err != nil {
		test.Fatal(err)
	}
	raw, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	fixture := &legacyFindGenerationFixture{generation: generation, owned: true, objects: map[domain.ContentHash][]byte{},
		binding: domain.DocReadGenerationBinding{SourceID: "11111111-1111-4111-8111-111111111111", RangeGeneration: domain.HashContent(raw)}}
	for _, object := range objects {
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	storage.fixtures[doc.Hash] = fixture
	return doc
}

func (storage *legacyFindGenerationStore) InReadOnlyTransaction(context.Context) bool {
	return storage.readonly
}

func (storage *legacyFindGenerationStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	storage.cost.opens++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fixture := storage.fixtures[hash]
	if repo != storage.repo || fixture == nil || !fixture.owned {
		return nil, domain.ErrNotFound
	}
	if fixture.failure != nil {
		return nil, fixture.failure
	}
	raw, _ := json.Marshal(fixture.generation)
	storage.cost.metadataBytes += len(raw)
	return &legacyFindGenerationRead{storage: storage, fixture: fixture}, nil
}

func (read *legacyFindGenerationRead) Descriptor() domain.DocRangeGeneration {
	return read.fixture.generation
}

func (read *legacyFindGenerationRead) Binding() domain.DocReadGenerationBinding {
	return read.fixture.binding
}

func (read *legacyFindGenerationRead) Envelope(ctx context.Context) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := read.Binding().Validate(); err != nil {
		return nil, err
	}
	return bytes.Clone(read.fixture.generation.Envelope), nil
}

func (read *legacyFindGenerationRead) PageHash(ctx context.Context, page int) (domain.ContentHash, error) {
	return domain.ResolveDocRangeLeaf(ctx, read.fixture.generation.EventPages, page, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		raw := read.fixture.objects[hash]
		read.storage.cost.directories++
		read.storage.cost.metadataBytes += len(raw)
		return raw, nil
	})
}

func (read *legacyFindGenerationRead) ReadPage(ctx context.Context, page int) ([]domain.DocRangeEntry, error) {
	read.storage.cost.pages++
	hash, err := read.PageHash(ctx, page)
	if err != nil {
		return nil, err
	}
	raw := read.fixture.objects[hash]
	read.storage.cost.metadataBytes += len(raw)
	return read.fixture.generation.DecodePage(page, hash, raw)
}

func (*legacyFindGenerationRead) Chunk(context.Context, int) (domain.ConversationManifestChunk, error) {
	return domain.ConversationManifestChunk{}, domain.ErrIntegrity
}

func (storage *legacyFindGenerationStore) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	storage.cost.whole++
	return storage.FSStore.GetDoc(ctx, repo, hash)
}

func (storage *legacyFindGenerationStore) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	storage.cost.indexes++
	return storage.FSStore.DocReadIndex(ctx, repo, hash)
}

func (storage *legacyFindGenerationStore) GetDocManifest(ctx context.Context, repo, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	storage.cost.manifests++
	manifest, err := storage.FSStore.GetDocManifest(ctx, repo, hash)
	raw, _ := json.Marshal(manifest)
	storage.cost.manifestBytes += len(raw)
	return manifest, err
}

func (storage *legacyFindGenerationStore) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	storage.cost.chunks++
	raw, err := storage.FSStore.GetChunk(ctx, repo, hash)
	storage.cost.chunkBytes += len(raw)
	return raw, err
}

func (storage *legacyFindGenerationStore) DocRangeBoundary(_ context.Context, repo, hash, base domain.ContentHash) (int, error) {
	storage.cost.boundaries++
	if repo != storage.repo || !storage.fixtures[hash].owned || !storage.fixtures[base].owned {
		return 0, domain.ErrNotFound
	}
	return storage.prefix, nil
}

func (storage *legacyFindGenerationStore) EnqueueDocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	storage.cost.writes++
	return domain.DocRangeIndexJob{}, domain.ErrIntegrity
}

func TestLegacyFindGenerationErrorsNeverSelectCompatibility(test *testing.T) {
	for _, failure := range []error{domain.ErrIntegrity, domain.ErrNotFound, domain.ErrForbidden, domain.ErrConflict, domain.ErrUnsupportedDocumentIdentity, context.Canceled, domain.ErrDocRangeIndexUnavailable} {
		test.Run(failure.Error(), func(test *testing.T) {
			storage := newLegacyFindGenerationStore(test)
			doc := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "needle")})
			storage.fixtures[doc.Hash].failure = failure
			service := NewService(storage, storage, nil, nil, nil)
			ctx := outbound.WithDocReadOnly(systemTestContext())
			page, err := service.FindDocPage(ctx, storage.repo, doc.Hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", To: 1, Total: 1})
			if failure == domain.ErrDocRangeIndexUnavailable {
				if err != nil || len(page.Matches) != 1 || storage.cost.indexes != 1 || storage.cost.whole != 0 {
					test.Fatal("genuinely absent generation lost explicit compatibility", page, storage.cost, err)
				}
				return
			}
			if !errors.Is(err, failure) || !reflect.DeepEqual(page, domain.DocFindPage{}) || storage.cost.indexes != 0 || storage.cost.manifests != 0 || storage.cost.whole != 0 {
				test.Fatal("generation failure fell back", page, storage.cost, err)
			}
			out, err := service.ReadDocFindFragment(ctx, storage.repo, doc.Hash, domain.DocFindFragmentRequest{Legacy: true, Index: 0, Total: 1, Budget: 64})
			if !errors.Is(err, failure) || out != (domain.DocFindFragmentPage{}) || storage.cost.indexes != 0 || storage.cost.whole != 0 {
				test.Fatal("fragment generation failure fell back", out, storage.cost, err)
			}
		})
	}
}

func TestLegacyFindGenerationRejectsMalformedMetadataAndUnpinnedReads(test *testing.T) {
	for _, mutation := range []string{"wrong hash", "root descriptor", "invalid binding", "corrupt directory", "corrupt page", "unpinned"} {
		test.Run(mutation, func(test *testing.T) {
			storage := newLegacyFindGenerationStore(test)
			doc := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "needle")})
			fixture := storage.fixtures[doc.Hash]
			want := domain.ErrIntegrity
			switch mutation {
			case "wrong hash":
				fixture.generation.Hash = h('9')
			case "root descriptor":
				fixture.generation.Version, fixture.generation.SourceIdentity = 2, nil
				fixture.generation.ChunkPages = domain.DocRangeDirectoryRoot{Hash: h('8'), Count: 1}
				if err := fixture.generation.Validate(doc.Hash); err != nil {
					test.Fatal("root descriptor fixture is malformed", err)
				}
			case "invalid binding":
				fixture.binding.SourceID = "00000000-0000-0000-0000-000000000000"
			case "corrupt directory":
				fixture.objects[fixture.generation.EventPages.Hash] = []byte("corrupt")
			case "corrupt page":
				reader := &legacyFindGenerationRead{storage: storage, fixture: fixture}
				hash, err := reader.PageHash(systemTestContext(), 0)
				if err != nil {
					test.Fatal(err)
				}
				fixture.objects[hash] = []byte("corrupt")
			case "unpinned":
				storage.readonly, want = false, domain.ErrConflict
			}
			storage.cost = legacyFindGenerationCosts{}
			service := NewService(storage, storage, nil, nil, nil)
			page, err := service.FindDocPage(systemTestContext(), storage.repo, doc.Hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", To: 1, Total: 1})
			if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.DocFindPage{}) || storage.cost.indexes != 0 || storage.cost.whole != 0 || storage.cost.manifests != 0 || storage.cost.writes != 0 {
				test.Fatal("malformed/unpinned metadata was accepted or bypassed", page, storage.cost, err)
			}
			fragment, err := service.ReadDocFindFragment(systemTestContext(), storage.repo, doc.Hash, domain.DocFindFragmentRequest{Legacy: true, Index: 0, Total: 1, Budget: 64})
			if !errors.Is(err, want) || fragment != (domain.DocFindFragmentPage{}) || storage.cost.indexes != 0 || storage.cost.whole != 0 || storage.cost.manifests != 0 {
				test.Fatal("fragment accepted or bypassed malformed metadata", fragment, storage.cost, err)
			}
		})
	}
}

func TestLegacyFindGenerationLargePublicFieldsDoNotExposeProviderBytes(test *testing.T) {
	storage := newLegacyFindGenerationStore(test)
	doc := storage.seed(test, []domain.CIREvent{
		{Kind: domain.EventReasoning, RedactedSummary: "public needle", Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: strings.Repeat("private ", 30_000)}},
		{Kind: domain.EventMessage, Role: domain.RoleAssistant, AgentMessage: true, Blocks: []domain.ContentBlock{{Type: "text", Text: "public needle"}}, Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: strings.Repeat("private ", 30_000)}},
	})
	service := NewService(storage, storage, nil, nil, nil)
	for _, representation := range []string{"", domain.DocFindDisplayRepresentation} {
		for _, query := range []string{"needle", "private"} {
			request := domain.DocFindPageRequest{Legacy: true, Query: query, Representation: representation, Total: 2, To: 2}
			matches := 0
			for attempt := 0; attempt < 4; attempt++ {
				page, err := service.FindDocPage(systemTestContext(), storage.repo, doc.Hash, request)
				if err != nil || page.Generation != "" {
					test.Fatal("prepared public-field compatibility failed", page, err)
				}
				matches += len(page.Matches)
				if page.NextCursor == nil {
					break
				}
				request = domain.DocFindPageRequest{Legacy: true, Query: query, Representation: representation, Cursor: *page.NextCursor}
			}
			want := 2
			if query == "private" {
				want = 0
			}
			if matches != want || storage.cost.indexes != 0 || storage.cost.whole != 0 {
				test.Fatal("provider-only bytes were searched or complete archive was loaded", query, matches, storage.cost)
			}
		}
	}
}

func TestLegacyFindGenerationSnapshotlessArchivesKeepTheirIdentity(test *testing.T) {
	for _, prepared := range []bool{false, true} {
		test.Run(fmt.Sprint(prepared), func(test *testing.T) {
			storage := newLegacyFindGenerationStore(test)
			doc := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "needle")})
			if !prepared {
				storage.fixtures[doc.Hash].failure = domain.ErrDocRangeIndexUnavailable
			}
			metadata := &legacyFindGenerationMeta{FSStore: storage.FSStore, failure: domain.ErrNotFound}
			service := NewService(metadata, storage, nil, nil, nil)
			ctx := outbound.WithDocReadOnly(systemTestContext())
			page, err := service.FindDocPage(ctx, storage.repo, doc.Hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", Total: 1, To: 1})
			if err != nil || len(page.Matches) != 1 || page.Generation != "" || page.Hash != doc.Hash || page.Projection != domain.DocLegacyFindProjection {
				test.Fatal("snapshotless legacy source changed identity or became unreadable", page, err)
			}
			fragment, err := service.ReadDocFindFragment(ctx, storage.repo, doc.Hash, domain.DocFindFragmentRequest{Legacy: true, Index: 0, Total: 1, Budget: 64})
			if err != nil || fragment.Generation != "" || fragment.Hash != doc.Hash || fragment.Projection != domain.DocLegacyFindFragmentProjection {
				test.Fatal("snapshotless legacy reveal changed identity or became unreadable", fragment, err)
			}
			if prepared && (storage.cost.indexes != 0 || storage.cost.whole != 0 || storage.cost.pages != 2) {
				test.Fatal("snapshotless prepared archive decoded its full legacy index", storage.cost)
			}
			storage.fixtures[doc.Hash].failure = domain.ErrNotFound
			page, err = service.FindDocPage(ctx, storage.repo, doc.Hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", Total: 1, To: 1})
			if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
				test.Fatal("snapshotless generation ownership denial was treated as absence", page, err)
			}
		})
	}
}

func TestLegacyFindGenerationRejectsWrongSnapshotBeforeCompatibility(test *testing.T) {
	for _, mutation := range []string{"hash", "missing ID", "repository", "identity"} {
		test.Run(mutation, func(test *testing.T) {
			storage := newLegacyFindGenerationStore(test)
			doc := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "needle")})
			snapshot := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: storage.repo}
			want := domain.ErrIntegrity
			switch mutation {
			case "hash":
				snapshot.ID, snapshot.DocHash = h('9'), h('9')
			case "missing ID":
				snapshot.ID = ""
			case "repository":
				snapshot.RepoID = h('9')
			case "identity":
				snapshot.DocIdentity, want = domain.DocumentIdentityRootV1, domain.ErrDocVisibleProjectionUnsupported
			}
			storage.fixtures[doc.Hash].failure = domain.ErrDocRangeIndexUnavailable
			service := NewService(&legacyFindGenerationMeta{FSStore: storage.FSStore, snapshot: snapshot}, storage, nil, nil, nil)
			page, err := service.FindDocPage(systemTestContext(), storage.repo, doc.Hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", Total: 1, To: 1})
			if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.DocFindPage{}) || storage.cost.indexes != 0 || storage.cost.opens != 0 || storage.cost.whole != 0 {
				test.Fatal("wrong snapshot selected legacy compatibility", page, storage.cost, err)
			}
			fragment, err := service.ReadDocFindFragment(systemTestContext(), storage.repo, doc.Hash, domain.DocFindFragmentRequest{Legacy: true, Index: 0, Total: 1, Budget: 64})
			if !errors.Is(err, want) || fragment != (domain.DocFindFragmentPage{}) || storage.cost.indexes != 0 || storage.cost.opens != 0 || storage.cost.whole != 0 {
				test.Fatal("wrong snapshot selected fragment compatibility", fragment, storage.cost, err)
			}
		})
	}
}
