package documentquery

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type readOnlyFixtureScope struct {
	fixture *conversationFindFixture
}

func (scope readOnlyFixtureScope) AuthorizeDocumentRead(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	return scope.fixture.AuthorizeDocumentRead(ctx, repo)
}

func (scope readOnlyFixtureScope) AuthorizeDocumentPreparation(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	scope.fixture.test.Fatal("read-only owner requested local preparation authorization")
	return ctx, domain.ErrForbidden
}

func (scope readOnlyFixtureScope) WithinDocumentRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope.WithinDocumentSnapshot(ctx, repo, read)
}

func (scope readOnlyFixtureScope) WithinDocumentSnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	bound, release, err := scope.fixture.AcquireInteractive(ctx)
	if err != nil {
		return err
	}
	defer release()
	return scope.fixture.WithinDocumentSnapshot(bound, repo, read)
}

type readOnlyCoreReads struct {
	fixture  *conversationFindFixture
	document domain.VerifiedSessionDoc
	verifier domain.CanonicalDocVerifier
}

func (reader *readOnlyCoreReads) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	reader.fixture.requireRead(ctx)
	if repo != reader.fixture.repo || hash != reader.fixture.hash {
		return domain.DocReadIndex{}, domain.ErrNotFound
	}
	plan, err := reader.document.PlanReadIndexContext(ctx)
	if err != nil {
		return domain.DocReadIndex{}, err
	}
	return plan.BuildContext(ctx, nil)
}

func (reader *readOnlyCoreReads) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, query string, after, limit int) ([]domain.DocEventIndex, error) {
	index, err := reader.DocReadIndex(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	return searchReadIndex(ctx, index, query, after, limit)
}

func (reader *readOnlyCoreReads) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	reader.fixture.requireRead(ctx)
	if repo != reader.fixture.repo || hash != reader.fixture.hash {
		return domain.VerifiedSessionDoc{}, domain.ErrNotFound
	}
	return reader.document, ctx.Err()
}

func (reader *readOnlyCoreReads) ReadVerifiedRootEvents(ctx context.Context, repo, hash domain.ContentHash, selection domain.RootEventSelection) (domain.VerifiedRootEvents, error) {
	reader.fixture.requireRead(ctx)
	if repo != reader.fixture.repo || hash != reader.fixture.hash {
		return domain.VerifiedRootEvents{}, domain.ErrNotFound
	}
	return reader.verifier.VerifyConversationEvents(ctx, hash, reader.fixture.manifest, func(read context.Context, chunk domain.ContentHash) ([]byte, error) {
		return reader.fixture.GetChunk(read, repo, chunk)
	}, selection)
}

type readOnlyBoundaryReads struct {
	fixture *conversationFindFixture
	failure error
	calls   int
}

func (reader *readOnlyBoundaryReads) InReadOnlyTransaction(ctx context.Context) bool {
	return reader.fixture.InReadOnlyTransaction(ctx)
}

func (reader *readOnlyBoundaryReads) DocRangeBoundary(ctx context.Context, repo, hash, base domain.ContentHash) (int, error) {
	reader.fixture.requireRead(ctx)
	reader.calls++
	if repo != reader.fixture.repo || hash != reader.fixture.hash || base != reader.fixture.hash {
		return 0, domain.ErrNotFound
	}
	return 0, reader.failure
}

type readOnlyByteReadiness struct {
	fixture *conversationFindFixture
	enabled bool
	ready   bool
	checks  int
}

func (reader *readOnlyByteReadiness) LegacyByteRangePublicationEnabled() bool {
	return reader.enabled
}

func (reader *readOnlyByteReadiness) HasDocByteRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	reader.fixture.requireRead(ctx)
	reader.checks++
	if repo != reader.fixture.repo || hash != reader.fixture.hash {
		return false, domain.ErrNotFound
	}
	return reader.ready, nil
}

type readOnlyFindFields struct {
	fixture *conversationFindFixture
	calls   int
}

func (reader *readOnlyFindFields) ReadDocFindFields(ctx context.Context, repo, hash, event domain.ContentHash, length int) (domain.DocFindFields, error) {
	reader.fixture.requireRead(ctx)
	reader.calls++
	if repo != reader.fixture.repo || hash != reader.fixture.hash || domain.ValidateContentHash(event) != nil || length <= 0 {
		return domain.DocFindFields{}, domain.ErrIntegrity
	}
	return domain.DocFindFields{}, domain.ErrNotFound
}

type readOnlyVisibleReadiness struct {
	DocVisibleReadiness
}

type readOnlyDisplayReads struct {
	outbound.DocDisplayReader
}

func newReadOnlyQueries(test *testing.T, legacy bool, events []domain.CIREvent) (*Queries, *conversationFindFixture) {
	test.Helper()
	_, fixture := newConversationFindFixture(test, legacy, events)
	reader := &readOnlyCoreReads{fixture: fixture}
	load := func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		return fixture.bodies[hash], nil
	}
	var err error
	if legacy {
		manifest := domain.DocChunkManifest{Format: domain.ChunkFormatV2, Envelope: fixture.manifest.Envelope}
		for _, chunk := range fixture.manifest.Chunks {
			manifest.Chunks = append(manifest.Chunks, chunk.Hash)
		}
		reader.document, err = reader.verifier.VerifyChunks(context.Background(), fixture.hash, manifest, load)
	} else {
		reader.document, err = reader.verifier.VerifyConversationManifestDoc(context.Background(), fixture.hash, fixture.manifest, load)
	}
	if err != nil {
		test.Fatal(err)
	}
	queries, err := New(Config{Profile: VerifiedRoots, ReadOnly: true, Scope: readOnlyFixtureScope{fixture: fixture},
		Snapshots: fixture, Legacy: fixture, Index: reader, Verified: reader, RootEvents: reader, AtomicRead: true,
		View: &ViewDependencies{Range: fixture, RangeGenerations: fixture, RangeReadiness: fixture,
			VisibleGenerations: fixture, VisibleReadiness: readOnlyVisibleReadiness{DocVisibleReadiness: fixture},
			BoundaryRead: &readOnlyBoundaryReads{fixture: fixture}, ByteReadiness: &readOnlyByteReadiness{fixture: fixture}, Chunks: fixture, Resources: fixture},
		Find: &FindDependencies{FieldsRead: &readOnlyFindFields{fixture: fixture}, DisplayRead: readOnlyDisplayReads{DocDisplayReader: fixture}}})
	if err != nil {
		test.Fatal(err)
	}
	return queries, fixture
}

func assertReadOnlyDependencies(test *testing.T, queries *Queries) {
	test.Helper()
	if !queries.readOnly || queries.view.RangeJobs != nil || queries.view.VisiblePreparation != nil || queries.view.LegacyPreparation != nil ||
		queries.view.Boundaries != nil || queries.view.Bytes != nil || queries.view.BoundaryPreparation != nil || queries.view.BytePreparation != nil ||
		queries.find.Fields != nil || queries.find.FieldsDemand != nil || queries.find.Display != nil {
		test.Fatal("read-only actual owner retained a local preparation dependency")
	}
	if _, mixed := queries.view.VisibleReadiness.(outbound.DocVisiblePreparationStore); mixed {
		test.Fatal("read-only visible readiness requires an enqueue-capable adapter")
	}
	if _, mixed := queries.view.ByteReadiness.(outbound.DocByteRangePreparationStore); mixed {
		test.Fatal("read-only byte readiness requires an enqueue-capable adapter")
	}
	if _, mixed := queries.view.BoundaryRead.(outbound.DocRangeBoundaryStore); mixed {
		test.Fatal("read-only boundaries require an enqueue-capable adapter")
	}
	if _, mixed := queries.find.FieldsRead.(FindFields); mixed {
		test.Fatal("read-only find fields require an enqueue-capable adapter")
	}
}
