package documentquery

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
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyFindGenerationCosts struct {
	opens, pages, directories, metadataBytes     int
	manifests, manifestBytes, chunks, chunkBytes int
	indexes, whole, boundaries, writes           int
}

type legacyFindGenerationFixture struct {
	generation domain.DocRangeGeneration
	binding    domain.DocReadGenerationBinding
	objects    map[domain.ContentHash][]byte
	owned      bool
}

type legacyFindGenerationStore struct {
	*store.FSStore
	repo     domain.ContentHash
	fixtures map[domain.ContentHash]*legacyFindGenerationFixture
	prefix   int
	cost     legacyFindGenerationCosts
}

type legacyFindGenerationRead struct {
	storage *legacyFindGenerationStore
	fixture *legacyFindGenerationFixture
}

type legacyFindGenerationMeta struct {
	failure error
}

type legacyFindGenerationMalformedStore struct {
	*legacyFindGenerationStore
	mutation string
}

type legacyFindGenerationMalformedRead struct {
	outbound.DocRangeGenerationRead
	storage *legacyFindGenerationMalformedStore
}

type legacyFindGenerationScope struct{}

func (legacyFindGenerationScope) AuthorizeDocumentRead(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	if authorize := inbound.DocumentReadAuthorizer(ctx); authorize != nil {
		return authorize(ctx, repo)
	}
	return ctx, ctx.Err()
}

func (legacyFindGenerationScope) AuthorizeDocumentPreparation(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, domain.ErrForbidden
}

func (scope legacyFindGenerationScope) WithinDocumentRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope.WithinDocumentSnapshot(ctx, repo, read)
}

func (scope legacyFindGenerationScope) WithinDocumentSnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	bound, err := scope.AuthorizeDocumentRead(ctx, repo)
	if err != nil {
		return err
	}
	return read(outbound.WithDocReadOnly(bound))
}

func (legacyFindGenerationScope) AcquireInteractive(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, ctx.Err()
}

func (legacyFindGenerationScope) AcquireBulk(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, ctx.Err()
}

func newLegacyFindGenerationQueries(test *testing.T, metadata SnapshotReader, storage *legacyFindGenerationStore, generations outbound.DocRangeGenerationStore) *Queries {
	test.Helper()
	scope := legacyFindGenerationScope{}
	queries, err := New(Config{Profile: LegacyCompatibility, Scope: scope, Snapshots: metadata, Legacy: storage, Index: storage,
		View: &ViewDependencies{RangeGenerations: generations, Boundaries: storage, Resources: scope}, Find: &FindDependencies{}})
	if err != nil {
		test.Fatal(err)
	}
	return queries
}

func legacyFindGenerationContext() context.Context {
	return inbound.WithSystemActor(context.Background())
}

func legacyFindGenerationMessage(role domain.Role, text string) domain.CIREvent {
	return domain.CIREvent{Kind: domain.EventMessage, Role: role, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}
}

func newLegacyFindGenerationStore(test *testing.T) *legacyFindGenerationStore {
	test.Helper()
	return &legacyFindGenerationStore{FSStore: store.NewFSStore(test.TempDir()), repo: domain.HashContent([]byte("legacy find generation repository")),
		fixtures: map[domain.ContentHash]*legacyFindGenerationFixture{}}
}

func (storage *legacyFindGenerationStore) seed(test *testing.T, events []domain.CIREvent) domain.SessionDoc {
	test.Helper()
	ctx := legacyFindGenerationContext()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex}, Events: events}}
	for index := range doc.CIR.Events {
		doc.CIR.Events[index].Seq = index
	}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		test.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := storage.PutDoc(ctx, storage.repo, doc); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, RepoID: storage.repo, DocHash: doc.Hash}); err != nil {
		test.Fatal(err)
	}
	verified, err := domain.VerifySessionDoc(doc)
	if err != nil {
		test.Fatal(err)
	}
	generation, objects, err := domain.BuildLegacyDocRangeGeneration(ctx, verified)
	if err != nil {
		test.Fatal(err)
	}
	raw, err = generation.Encode()
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

func (*legacyFindGenerationStore) InReadOnlyTransaction(ctx context.Context) bool {
	return outbound.DocReadOnly(ctx)
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

func (metadata *legacyFindGenerationMeta) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	return domain.Snapshot{}, metadata.failure
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

func TestLegacyFindGenerationMetadataAndFragmentCostsStaySelected(test *testing.T) {
	var costs []legacyFindGenerationCosts
	for _, count := range []int{2048, 20480} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			storage := newLegacyFindGenerationStore(test)
			events := make([]domain.CIREvent, count)
			for index := range events {
				events[index] = legacyFindGenerationMessage(domain.RoleUser, fmt.Sprintf("source row %d", index))
			}
			doc := storage.seed(test, events)
			queries := newLegacyFindGenerationQueries(test, storage, storage, storage)
			ctx := outbound.WithDocReadOnly(legacyFindGenerationContext())
			source, err := queries.legacyFindSource(ctx, storage.repo, doc.Hash)
			if err != nil || source.root || source.total != count || source.binding != storage.fixtures[doc.Hash].binding {
				test.Fatal("prepared legacy source changed identity or lost binding", source.binding, err)
			}
			entry, err := source.entry(128)
			if err != nil || entry.Index != 128 || storage.cost.pages != 1 || storage.cost.manifests != 0 || storage.cost.chunks != 0 || storage.cost.indexes != 0 {
				test.Fatal("metadata lookup loaded archival bytes or full index", storage.cost, err)
			}
			storage.cost = legacyFindGenerationCosts{}
			page, err := queries.FindDocPage(ctx, storage.repo, doc.Hash, domain.DocFindPageRequest{Legacy: true, Query: "absent", From: 128, To: count, Total: count})
			if err != nil || page.Index != 256 || page.NextCursor == nil || len(page.Matches) != 0 || page.Generation != "" || page.Hash != doc.Hash {
				test.Fatal("prepared legacy scan lost its bounded continuation", page, err)
			}
			cost := storage.cost
			if cost.opens != 1 || cost.pages != 1 || cost.directories > 2 || cost.metadataBytes > domain.MaxDocRangeGenerationBytes+domain.MaxDocRangePageBytes+2*domain.MaxDocRangeDirectoryBytes ||
				cost.manifests != 1 || cost.chunks != 1 || cost.chunkBytes > domain.ChunkTarget || cost.indexes != 0 || cost.whole != 0 || cost.writes != 0 {
				test.Fatal("fixed selected range scaled with archive", cost)
			}
			costs = append(costs, cost)
			test.Logf("events=%d find cost=%+v", count, cost)
			storage.cost = legacyFindGenerationCosts{}
			fragment, err := queries.ReadDocFindFragment(ctx, storage.repo, doc.Hash, domain.DocFindFragmentRequest{Legacy: true, Index: 128, Total: count, Budget: 64})
			if err != nil || fragment.Hash != doc.Hash || fragment.Generation != "" || fragment.Projection != domain.DocLegacyFindFragmentProjection || fragment.Index != 128 || len(fragment.JSON) != 64 {
				test.Fatal("prepared fragment changed source coordinates", fragment, err)
			}
			if storage.cost.opens != 1 || storage.cost.pages != 1 || storage.cost.directories > 2 || storage.cost.manifests != 1 || storage.cost.chunks != 1 || storage.cost.indexes != 0 || storage.cost.whole != 0 || storage.cost.writes != 0 {
				test.Fatal("fragment bypassed selected metadata", storage.cost)
			}
			test.Logf("events=%d fragment cost=%+v", count, storage.cost)
		})
	}
	if len(costs) == 2 && (costs[1].metadataBytes >= costs[0].metadataBytes*2 || costs[1].manifestBytes <= costs[0].manifestBytes) {
		test.Fatal("selected metadata must remain bounded, but archival manifest cost must remain visible", costs)
	}
}

func TestLegacyFindGenerationDurableBaseAndFreshOwnership(test *testing.T) {
	storage := newLegacyFindGenerationStore(test)
	shared := legacyFindGenerationMessage(domain.RoleUser, "shared needle")
	base := storage.seed(test, []domain.CIREvent{shared})
	doc := storage.seed(test, []domain.CIREvent{shared, legacyFindGenerationMessage(domain.RoleAssistant, strings.Repeat("Needle🙂 ", 45))})
	storage.prefix = 1
	queries := newLegacyFindGenerationQueries(test, storage, storage, storage)
	ctx := outbound.WithDocReadOnly(legacyFindGenerationContext())
	request := domain.DocFindPageRequest{Legacy: true, Query: "needle", Base: base.Hash, From: 1, To: 2, Total: 2, Inherited: 1}
	first, err := queries.FindDocPage(ctx, storage.repo, doc.Hash, request)
	if err != nil || first.NextCursor == nil || len(first.Matches) != domain.DocFindPageMatches || first.Generation != "" || storage.cost.boundaries != 1 || storage.cost.pages != 1 || storage.cost.manifests != 1 {
		test.Fatal("warm pair replayed inherited prefix or changed projection", first, storage.cost, err)
	}
	continuation := domain.DocFindPageRequest{Legacy: true, Query: "needle", Cursor: *first.NextCursor}
	storage.fixtures[doc.Hash].binding.SourceVersion++
	source, err := queries.legacyFindSource(ctx, storage.repo, doc.Hash)
	if err != nil || source.binding != storage.fixtures[doc.Hash].binding {
		test.Fatal("warm source reused a previous incarnation", source.binding, err)
	}
	for _, revoked := range []domain.ContentHash{doc.Hash, base.Hash} {
		storage.fixtures[revoked].owned = false
		for _, selected := range []domain.DocFindPageRequest{request, continuation} {
			out, err := queries.FindDocPage(ctx, storage.repo, doc.Hash, selected)
			if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(out, domain.DocFindPage{}) || storage.cost.indexes != 0 || storage.cost.whole != 0 {
				test.Fatal("warm find bypassed current source/base ownership", revoked, out, storage.cost, err)
			}
		}
		if revoked == doc.Hash {
			out, err := queries.ReadDocFindFragment(ctx, storage.repo, doc.Hash, domain.DocFindFragmentRequest{Legacy: true, Index: 1, Total: 2, Budget: 64})
			if !errors.Is(err, domain.ErrNotFound) || out != (domain.DocFindFragmentPage{}) {
				test.Fatal("warm fragment bypassed ownership", out, err)
			}
		}
		storage.fixtures[revoked].owned = true
	}
	before := storage.cost
	denied := inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		return bound, domain.ErrForbidden
	})
	if _, err := queries.FindDocPage(denied, storage.repo, doc.Hash, continuation); !errors.Is(err, domain.ErrForbidden) || storage.cost != before {
		test.Fatal("denied continuation touched a prepared source", storage.cost, err)
	}
	if _, err := queries.ReadDocFindFragment(denied, storage.repo, doc.Hash, domain.DocFindFragmentRequest{Legacy: true, Index: 1, Total: 2, Budget: 64}); !errors.Is(err, domain.ErrForbidden) || storage.cost != before {
		test.Fatal("denied fragment touched a prepared source", storage.cost, err)
	}
}

func TestLegacyFindGenerationSharedMetadataRejectsMalformedPagesBeforeCaching(test *testing.T) {
	for _, snapshotless := range []bool{false, true} {
		for _, mutation := range []string{"short page", "empty page", "wrong position", "read failure"} {
			test.Run(fmt.Sprintf("snapshotless=%t/%s", snapshotless, mutation), func(test *testing.T) {
				storage := &legacyFindGenerationMalformedStore{legacyFindGenerationStore: newLegacyFindGenerationStore(test), mutation: mutation}
				doc := storage.seed(test, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "needle"), legacyFindGenerationMessage(domain.RoleAssistant, "second")})
				var metadata SnapshotReader = storage.FSStore
				if snapshotless {
					metadata = &legacyFindGenerationMeta{failure: domain.ErrNotFound}
				}
				queries := newLegacyFindGenerationQueries(test, metadata, storage.legacyFindGenerationStore, storage)
				ctx := outbound.WithDocReadOnly(legacyFindGenerationContext())
				page, err := queries.FindDocPage(ctx, storage.repo, doc.Hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", Total: 2, To: 1})
				if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
					test.Fatal("normal/snapshotless find accepted malformed metadata page", page, err)
				}
				fragment, err := queries.ReadDocFindFragment(ctx, storage.repo, doc.Hash, domain.DocFindFragmentRequest{Legacy: true, Index: 0, Total: 2, Budget: 64})
				if !errors.Is(err, domain.ErrIntegrity) || fragment != (domain.DocFindFragmentPage{}) || storage.cost.indexes != 0 || storage.cost.whole != 0 || storage.cost.manifests != 0 {
					test.Fatal("normal/snapshotless reveal accepted malformed metadata page or fell back", fragment, storage.cost, err)
				}
				source, err := queries.legacyFindSource(ctx, storage.repo, doc.Hash)
				if err != nil {
					test.Fatal(err)
				}
				if _, err := source.location(0); !errors.Is(err, domain.ErrIntegrity) {
					test.Fatal("metadata location accepted malformed page", err)
				}
				reads := storage.cost.pages
				storage.mutation = ""
				entry, err := source.entry(0)
				if err != nil || entry.Index != 0 || storage.cost.pages != reads+1 || source.binding != storage.fixtures[doc.Hash].binding {
					test.Fatal("failed metadata page entered the cache or changed binding", entry, storage.cost, err)
				}
				if _, err := source.location(1); err != nil || storage.cost.pages != reads+1 || storage.cost.manifests != 0 {
					test.Fatal("valid metadata cache was bypassed or read archival bytes", storage.cost, err)
				}
			})
		}
	}
}
