package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave58PublicationStore struct {
	getRepo       func(context.Context, domain.ContentHash) (domain.Repo, error)
	listSnapshots func(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error)
	getSettings   func(context.Context, domain.ContentHash, domain.ContentHash) (domain.SettingsBundle, error)
	getSnapshot   func(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
	putSnapshot   func(context.Context, domain.Snapshot) error
}

func (store *wave58PublicationStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return store.getRepo(ctx, repo)
}

func (store *wave58PublicationStore) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	return store.listSnapshots(ctx, repo, branch)
}

func (store *wave58PublicationStore) GetSettingsObject(ctx context.Context, repo, hash domain.ContentHash) (domain.SettingsBundle, error) {
	return store.getSettings(ctx, repo, hash)
}

func (store *wave58PublicationStore) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	return store.getSnapshot(ctx, repo, hash)
}

func (store *wave58PublicationStore) PutSnapshot(ctx context.Context, snapshot domain.Snapshot) error {
	return store.putSnapshot(ctx, snapshot)
}

type wave58PublicationChunks struct {
	get func(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error)
	put func(context.Context, domain.ContentHash, map[domain.ContentHash][]byte) (int, int, error)
}

func (chunks *wave58PublicationChunks) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	return chunks.get(ctx, repo, hash)
}

func (chunks *wave58PublicationChunks) PutChunks(ctx context.Context, repo domain.ContentHash, bodies map[domain.ContentHash][]byte) (int, int, error) {
	return chunks.put(ctx, repo, bodies)
}

type wave58PublicationSources struct {
	verify func(context.Context, domain.ContentHash, domain.Snapshot) error
}

func (sources *wave58PublicationSources) VerifySnapshotDocument(ctx context.Context, repo domain.ContentHash, snapshot domain.Snapshot) error {
	return sources.verify(ctx, repo, snapshot)
}

type wave58PublicationVerified struct {
	put func(context.Context, domain.ContentHash, domain.VerifiedSessionDoc) (bool, error)
}

func (writer *wave58PublicationVerified) PutVerifiedDoc(ctx context.Context, repo domain.ContentHash, document domain.VerifiedSessionDoc) (bool, error) {
	return writer.put(ctx, repo, document)
}

type wave58PublicationLegacy struct {
	put func(context.Context, domain.ContentHash, domain.SessionDoc) (bool, error)
}

func (writer *wave58PublicationLegacy) PutDoc(ctx context.Context, repo domain.ContentHash, document domain.SessionDoc) (bool, error) {
	return writer.put(ctx, repo, document)
}

type wave58PublicationScope struct {
	commit  func(context.Context, domain.ContentHash, func(context.Context) (inbound.CommitOutput, error)) (inbound.CommitOutput, error)
	chunks  func(context.Context, domain.ContentHash, func(context.Context) (inbound.StoreChunksOutput, error)) (inbound.StoreChunksOutput, error)
	changed func(context.Context, domain.ContentHash) error
}

func (scope *wave58PublicationScope) WithinDocumentCommit(ctx context.Context, repo domain.ContentHash, operation func(context.Context) (inbound.CommitOutput, error)) (inbound.CommitOutput, error) {
	return scope.commit(ctx, repo, operation)
}

func (scope *wave58PublicationScope) WithinChunkStore(ctx context.Context, repo domain.ContentHash, operation func(context.Context) (inbound.StoreChunksOutput, error)) (inbound.StoreChunksOutput, error) {
	return scope.chunks(ctx, repo, operation)
}

func (scope *wave58PublicationScope) GraphChanged(ctx context.Context, repo domain.ContentHash) error {
	return scope.changed(ctx, repo)
}

var (
	_ DocumentPublicationStore            = (*wave58PublicationStore)(nil)
	_ DocumentPublicationChunkStore       = (*wave58PublicationChunks)(nil)
	_ DocumentPublicationLegacyWriter     = (*wave58PublicationLegacy)(nil)
	_ DocumentPublicationScope            = (*wave58PublicationScope)(nil)
	_ outbound.VerifiedDocStore           = (*wave58PublicationVerified)(nil)
	_ inbound.DocumentPublicationCommands = (*DocumentPublicationCommands)(nil)
)

type wave58PublicationCallerKey struct{}
type wave58PublicationBoundKey struct{}

type wave58PublicationFixture struct {
	test            *testing.T
	ctx             context.Context
	repo            domain.ContentHash
	metadata        domain.Repo
	config          DocumentPublicationCommandsConfig
	store           *wave58PublicationStore
	chunkStore      *wave58PublicationChunks
	sources         *wave58PublicationSources
	verified        *wave58PublicationVerified
	legacy          *wave58PublicationLegacy
	scope           *wave58PublicationScope
	snapshots       map[domain.ContentHash]domain.Snapshot
	documents       map[domain.ContentHash][]byte
	chunks          map[domain.ContentHash][]byte
	settings        map[domain.ContentHash]domain.SettingsBundle
	proofs          []domain.VerifiedSessionDoc
	legacyDocuments []domain.SessionDoc
	events          []string
	chunkReads      []domain.ContentHash
	settingsReads   []domain.ContentHash
	sourceChecks    []domain.Snapshot
	active          bool
	commitScopes    int
	chunkScopes     int
	callbacks       int
	verifiedWrites  int
	legacyWrites    int
	chunkWrites     int
	snapshotWrites  int
	graphChanges    int
	entryErr        error
	exitErr         error
	sourceErr       error
	verifiedErr     error
	legacyErr       error
	chunkReadErr    error
	chunkWriteErr   error
	graphErr        error
	beforeCallback  func()
	afterChunkRead  func()
}

func (fixture *wave58PublicationFixture) checkContext(ctx context.Context, bound bool) {
	fixture.test.Helper()
	actor, system := inbound.RepositoryActor(ctx)
	if actor != "publication-owner" || system || ctx.Value(wave58PublicationCallerKey{}) != fixture || inbound.Correlation(ctx) != "wave58-publication" {
		fixture.test.Fatal("caller identity or correlation was discarded")
	}
	deadline, hasDeadline := ctx.Deadline()
	wantDeadline, wantHasDeadline := fixture.ctx.Deadline()
	if hasDeadline != wantHasDeadline || !deadline.Equal(wantDeadline) {
		fixture.test.Fatal("caller deadline changed")
	}
	if bound && (!fixture.active || ctx.Value(wave58PublicationBoundKey{}) != fixture) {
		fixture.test.Fatal("publication port escaped the bound scope")
	}
}

func (fixture *wave58PublicationFixture) observe(ctx context.Context, repo domain.ContentHash, event string) error {
	fixture.test.Helper()
	fixture.checkContext(ctx, true)
	if repo != fixture.repo {
		fixture.test.Fatalf("%s used repository %s, want %s", event, repo, fixture.repo)
	}
	fixture.events = append(fixture.events, event)
	return ctx.Err()
}

func (fixture *wave58PublicationFixture) enter(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	fixture.test.Helper()
	fixture.checkContext(ctx, false)
	if repo != fixture.repo {
		fixture.test.Fatalf("scope repository = %s, want %s", repo, fixture.repo)
	}
	if fixture.entryErr != nil {
		return nil, fixture.entryErr
	}
	fixture.active = true
	fixture.callbacks++
	if fixture.beforeCallback != nil {
		fixture.beforeCallback()
	}
	return context.WithValue(ctx, wave58PublicationBoundKey{}, fixture), nil
}

func wave58PublicationDocument(test *testing.T, marker string, events ...domain.CIREvent) domain.SessionDoc {
	test.Helper()
	if len(events) == 0 {
		events = []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: marker}}}}
	}
	document := domain.SessionDoc{CIR: domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: domain.CIRVersionV2, SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull, GitBranch: "main", SessionOriginID: marker},
		Events:   events,
	}}
	document.Hash = domain.HashContent(wave58PublicationBytes(test, document))
	return document
}

func wave58PublicationBytes(test *testing.T, document domain.SessionDoc) []byte {
	test.Helper()
	body, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	return body
}

func (fixture *wave58PublicationFixture) assertNoWrites() {
	fixture.test.Helper()
	if fixture.verifiedWrites != 0 || fixture.legacyWrites != 0 || fixture.chunkWrites != 0 || fixture.snapshotWrites != 0 || fixture.graphChanges != 0 {
		fixture.test.Fatalf("validation wrote objects: verified=%d legacy=%d chunks=%d snapshots=%d graph=%d", fixture.verifiedWrites, fixture.legacyWrites, fixture.chunkWrites, fixture.snapshotWrites, fixture.graphChanges)
	}
}
