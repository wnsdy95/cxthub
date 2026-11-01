package documentworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave38DisplayWorkKey struct{}
type wave38DisplayReadKey struct{}

type wave38DisplayScope struct {
	test                   *testing.T
	workers, works, reads  int
	activeWork, activeRead int
	workError, readError   error
	afterRead              error
}

func (scope *wave38DisplayScope) WorkerContext(ctx context.Context) context.Context {
	scope.workers++
	return ctx
}

func (scope *wave38DisplayScope) WithinWork(ctx context.Context, work func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope.workError != nil {
		return scope.workError
	}
	scope.works++
	scope.activeWork++
	defer func() { scope.activeWork-- }()
	return work(context.WithValue(ctx, wave38DisplayWorkKey{}, true))
}

func (scope *wave38DisplayScope) WithinRepositoryRead(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope.readError != nil {
		return scope.readError
	}
	scope.reads++
	scope.activeRead++
	bound, cancel := context.WithCancel(context.WithValue(ctx, wave38DisplayReadKey{}, true))
	defer cancel()
	defer func() { scope.activeRead-- }()
	return errors.Join(read(bound), scope.afterRead)
}

func (scope *wave38DisplayScope) CanYield(context.Context) bool { return false }

func (scope *wave38DisplayScope) Yield(context.Context, outbound.RootPublicationResources) error {
	scope.test.Fatal("display work introduced a publication yield")
	return domain.ErrIntegrity
}

func (scope *wave38DisplayScope) requireRead(ctx context.Context) {
	scope.test.Helper()
	if scope.activeRead != 1 || ctx.Value(wave38DisplayReadKey{}) != true || ctx.Err() != nil {
		scope.test.Fatal("source escaped its live repository read", scope.activeRead, ctx.Err())
	}
}

func (scope *wave38DisplayScope) requireWrite(ctx context.Context) {
	scope.test.Helper()
	if scope.activeRead != 0 || ctx.Value(wave38DisplayReadKey{}) != nil {
		scope.test.Fatal("durable progress was written inside its source read")
	}
}

func (scope *wave38DisplayScope) AuthorizeDocumentRead(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, ctx.Err()
}

func (scope *wave38DisplayScope) AuthorizeDocumentPreparation(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	scope.test.Fatal("worker source attempted query preparation")
	return ctx, domain.ErrIntegrity
}

func (scope *wave38DisplayScope) WithinDocumentRead(context.Context, domain.ContentHash, func(context.Context) error) error {
	scope.test.Fatal("source opened a second read scope")
	return domain.ErrIntegrity
}

func (scope *wave38DisplayScope) WithinDocumentSnapshot(context.Context, domain.ContentHash, func(context.Context) error) error {
	scope.test.Fatal("source opened a second snapshot scope")
	return domain.ErrIntegrity
}

func (scope *wave38DisplayScope) AcquireInteractive(ctx context.Context) (context.Context, func(), error) {
	scope.requireRead(ctx)
	return ctx, func() {}, nil
}

func (scope *wave38DisplayScope) AcquireBulk(ctx context.Context) (context.Context, func(), error) {
	return scope.AcquireInteractive(ctx)
}

type wave38DisplayFixture struct {
	test            *testing.T
	scope           *wave38DisplayScope
	repo, hash      domain.ContentHash
	binding         domain.DocReadGenerationBinding
	manifest        domain.ConversationManifest
	generation      domain.DocRangeGeneration
	pages           [][]domain.DocRangeEntry
	pageHashes      []domain.ContentHash
	bodies          map[domain.ContentHash][]byte
	metadata, opens int
	pageReads       int
}

func newWave38DisplayFixture(test *testing.T, events []domain.CIREvent) (*wave38DisplayFixture, *documentquery.Queries) {
	test.Helper()
	for index := range events {
		events[index].Seq = index
	}
	ctx := context.Background()
	manifest, bodies, err := domain.ConversationManifestForCIR(domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex}, Events: events,
	})
	if err != nil {
		test.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		test.Fatal(err)
	}
	var verifier domain.CanonicalDocVerifier
	verified, err := verifier.VerifyConversationManifestDoc(ctx, hash, manifest, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		return bodies[hash], nil
	})
	if err != nil {
		test.Fatal(err)
	}
	index, err := domain.BuildDocRangeIndex(ctx, verified)
	if err != nil {
		test.Fatal(err)
	}
	proof, _, err := domain.BuildVerifiedDocRangeGeneration(ctx, index)
	if err != nil {
		test.Fatal(err)
	}
	generation := proof.Descriptor()
	encoded, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	fixture := &wave38DisplayFixture{test: test, scope: &wave38DisplayScope{test: test}, repo: domain.HashContent([]byte("wave38 display repo")),
		hash: hash, binding: domain.DocReadGenerationBinding{SourceID: "11111111-1111-4111-8111-111111111111", RangeGeneration: domain.HashContent(encoded)},
		manifest: manifest, generation: generation, bodies: bodies, pageHashes: index.Header().Pages}
	for page := range fixture.pageHashes {
		raw, err := index.Page(ctx, page)
		if err != nil {
			test.Fatal(err)
		}
		entries, err := index.Header().DecodePage(page, raw)
		if err != nil {
			test.Fatal(err)
		}
		fixture.pages = append(fixture.pages, entries)
	}
	queries, err := documentquery.New(documentquery.Config{Profile: documentquery.LegacyCompatibility, Scope: fixture.scope, Snapshots: fixture, Legacy: fixture,
		View: &documentquery.ViewDependencies{Range: fixture, RangeGenerations: fixture, Chunks: fixture, Resources: fixture.scope}})
	if err != nil {
		test.Fatal(err)
	}
	return fixture, queries
}

func (fixture *wave38DisplayFixture) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	fixture.scope.requireRead(ctx)
	fixture.metadata++
	if repo != fixture.repo || hash != fixture.hash {
		return domain.Snapshot{}, domain.ErrNotFound
	}
	return domain.Snapshot{ID: hash, RepoID: repo, DocHash: hash, DocIdentity: domain.DocumentIdentityRootV1}, nil
}

func (fixture *wave38DisplayFixture) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	fixture.test.Fatal("root worker materialized a legacy document")
	return domain.SessionDoc{}, domain.ErrIntegrity
}

func (fixture *wave38DisplayFixture) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	fixture.test.Fatal("root worker opened a legacy manifest")
	return domain.DocChunkManifest{}, domain.ErrIntegrity
}

func (fixture *wave38DisplayFixture) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	fixture.test.Fatal("root worker read a legacy chunk")
	return nil, domain.ErrIntegrity
}

func (fixture *wave38DisplayFixture) DocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocRangeIndex, error) {
	fixture.test.Fatal("root worker bypassed its pinned generation")
	return domain.DocRangeIndex{}, domain.ErrIntegrity
}

func (fixture *wave38DisplayFixture) DocRangePage(context.Context, domain.ContentHash, domain.DocRangeIndex, int) ([]domain.DocRangeEntry, error) {
	fixture.test.Fatal("root worker bypassed its pinned generation page")
	return nil, domain.ErrIntegrity
}

func (fixture *wave38DisplayFixture) InReadOnlyTransaction(ctx context.Context) bool {
	return fixture.scope.activeRead == 1 && ctx.Value(wave38DisplayReadKey{}) == true && ctx.Err() == nil
}

func (fixture *wave38DisplayFixture) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	fixture.scope.requireRead(ctx)
	if repo != fixture.repo || hash != fixture.hash {
		return nil, domain.ErrNotFound
	}
	fixture.opens++
	return fixture, nil
}

func (fixture *wave38DisplayFixture) Descriptor() domain.DocRangeGeneration {
	return fixture.generation
}
func (fixture *wave38DisplayFixture) Binding() domain.DocReadGenerationBinding {
	return fixture.binding
}

func (fixture *wave38DisplayFixture) Envelope(ctx context.Context) (json.RawMessage, error) {
	fixture.scope.requireRead(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := fixture.Binding().Validate(); err != nil {
		return nil, err
	}
	return bytes.Clone(fixture.generation.Envelope), nil
}

func (fixture *wave38DisplayFixture) PageHash(ctx context.Context, page int) (domain.ContentHash, error) {
	fixture.scope.requireRead(ctx)
	return fixture.pageHashes[page], nil
}

func (fixture *wave38DisplayFixture) ReadPage(ctx context.Context, page int) ([]domain.DocRangeEntry, error) {
	fixture.scope.requireRead(ctx)
	fixture.pageReads++
	return fixture.pages[page], nil
}

func (fixture *wave38DisplayFixture) Chunk(ctx context.Context, position int) (domain.ConversationManifestChunk, error) {
	fixture.scope.requireRead(ctx)
	return fixture.manifest.Chunks[position], nil
}

func (fixture *wave38DisplayFixture) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	fixture.scope.requireRead(ctx)
	raw := fixture.bodies[hash]
	if repo != fixture.repo || int64(len(raw)) != length {
		return nil, domain.ErrIntegrity
	}
	return raw, nil
}

func (fixture *wave38DisplayFixture) target(index int, profile string) domain.DocDisplayTarget {
	return domain.DocDisplayTarget{RepoID: fixture.repo, DocHash: fixture.hash, Binding: fixture.binding,
		Event: fixture.pages[index/domain.DocRangePageEvents][index%domain.DocRangePageEvents].DocEventIndex, Profile: profile}
}

type wave38DisplaySources struct {
	queries       *documentquery.Queries
	scope         *wave38DisplayScope
	display, view int
	last          documentquery.Source
}

func (sources *wave38DisplaySources) OpenSource(ctx context.Context, repo, hash domain.ContentHash) (documentquery.Source, error) {
	sources.scope.requireRead(ctx)
	sources.display++
	source, err := sources.queries.OpenSource(ctx, repo, hash)
	sources.last = source
	return source, err
}

func (sources *wave38DisplaySources) OpenViewSource(ctx context.Context, repo, hash domain.ContentHash) (documentquery.Source, error) {
	sources.scope.requireRead(ctx)
	sources.view++
	source, err := sources.queries.OpenViewSource(ctx, repo, hash)
	sources.last = source
	return source, err
}
