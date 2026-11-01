package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

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
	_ DocumentPublicationSources          = (*wave58PublicationSources)(nil)
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

func wave58PublicationHash(value string) domain.ContentHash {
	return domain.HashContent([]byte("wave58-publication/" + value))
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

func newWave58PublicationFixture(test *testing.T) *wave58PublicationFixture {
	test.Helper()
	fixture := &wave58PublicationFixture{
		test: test, repo: wave58PublicationHash(test.Name()),
		snapshots: make(map[domain.ContentHash]domain.Snapshot), documents: make(map[domain.ContentHash][]byte),
		chunks: make(map[domain.ContentHash][]byte), settings: make(map[domain.ContentHash]domain.SettingsBundle),
	}
	ctx := context.WithValue(context.Background(), wave58PublicationCallerKey{}, fixture)
	ctx = inbound.WithCorrelation(inbound.WithRepositoryActor(ctx, "publication-owner"), "wave58-publication")
	ctx = inbound.WithDocumentIdentities(ctx, []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1})
	bound, cancel := context.WithTimeout(ctx, time.Minute)
	test.Cleanup(cancel)
	fixture.ctx = bound
	fixture.metadata = domain.Repo{ID: fixture.repo, RepositoryID: "publication-repository", DefaultBranch: "main"}
	fixture.scope = &wave58PublicationScope{}
	fixture.scope.commit = func(ctx context.Context, repo domain.ContentHash, operation func(context.Context) (inbound.CommitOutput, error)) (inbound.CommitOutput, error) {
		fixture.commitScopes++
		bound, err := fixture.enter(ctx, repo)
		if err != nil {
			return inbound.CommitOutput{}, err
		}
		defer func() { fixture.active = false }()
		output, err := operation(bound)
		if err != nil {
			return output, err
		}
		return output, fixture.exitErr
	}
	fixture.scope.chunks = func(ctx context.Context, repo domain.ContentHash, operation func(context.Context) (inbound.StoreChunksOutput, error)) (inbound.StoreChunksOutput, error) {
		fixture.chunkScopes++
		bound, err := fixture.enter(ctx, repo)
		if err != nil {
			return inbound.StoreChunksOutput{}, err
		}
		defer func() { fixture.active = false }()
		output, err := operation(bound)
		if err != nil {
			return output, err
		}
		return output, fixture.exitErr
	}
	fixture.scope.changed = func(ctx context.Context, repo domain.ContentHash) error {
		if err := fixture.observe(ctx, repo, "graph-changed"); err != nil {
			return err
		}
		fixture.graphChanges++
		return fixture.graphErr
	}
	fixture.store = &wave58PublicationStore{
		getRepo: func(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
			if err := fixture.observe(ctx, repo, "get-repo"); err != nil {
				return domain.Repo{}, err
			}
			return fixture.metadata, nil
		},
		listSnapshots: func(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
			if err := fixture.observe(ctx, repo, "list-snapshots"); err != nil {
				return nil, err
			}
			if branch != "" {
				test.Fatal("graph preflight requested a branch-filtered graph")
			}
			var snapshots []domain.Snapshot
			for _, snapshot := range fixture.snapshots {
				snapshots = append(snapshots, snapshot)
			}
			return snapshots, nil
		},
		getSettings: func(ctx context.Context, repo, hash domain.ContentHash) (domain.SettingsBundle, error) {
			if err := fixture.observe(ctx, repo, "get-settings"); err != nil {
				return domain.SettingsBundle{}, err
			}
			fixture.settingsReads = append(fixture.settingsReads, hash)
			bundle, exists := fixture.settings[hash]
			if !exists {
				return domain.SettingsBundle{}, domain.ErrNotFound
			}
			return bundle, nil
		},
		getSnapshot: func(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
			if err := fixture.observe(ctx, repo, "get-snapshot"); err != nil {
				return domain.Snapshot{}, err
			}
			snapshot, exists := fixture.snapshots[hash]
			if !exists {
				return domain.Snapshot{}, domain.ErrNotFound
			}
			return snapshot, nil
		},
		putSnapshot: func(ctx context.Context, snapshot domain.Snapshot) error {
			if err := fixture.observe(ctx, snapshot.RepoID, "put-snapshot"); err != nil {
				return err
			}
			fixture.snapshotWrites++
			if current, exists := fixture.snapshots[snapshot.ID]; exists {
				if current.DocumentRef() != snapshot.DocumentRef() {
					return domain.ErrIntegrity
				}
				if current.Branch == domain.StashBranchLabel && snapshot.Branch != domain.StashBranchLabel {
					current.Branch, current.Message = snapshot.Branch, snapshot.Message
					fixture.snapshots[snapshot.ID] = current
				}
				return nil
			}
			fixture.snapshots[snapshot.ID] = snapshot
			return nil
		},
	}
	fixture.chunkStore = &wave58PublicationChunks{
		get: func(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
			if err := fixture.observe(ctx, repo, "get-chunk"); err != nil {
				return nil, err
			}
			fixture.chunkReads = append(fixture.chunkReads, hash)
			if fixture.afterChunkRead != nil {
				fixture.afterChunkRead()
			}
			if fixture.chunkReadErr != nil {
				return nil, fixture.chunkReadErr
			}
			body, exists := fixture.chunks[hash]
			if !exists {
				return nil, domain.ErrNotFound
			}
			return append([]byte(nil), body...), nil
		},
		put: func(ctx context.Context, repo domain.ContentHash, bodies map[domain.ContentHash][]byte) (int, int, error) {
			if err := fixture.observe(ctx, repo, "put-chunks"); err != nil {
				return 0, 0, err
			}
			fixture.chunkWrites++
			if fixture.chunkWriteErr != nil {
				return 9, 7, fixture.chunkWriteErr
			}
			stored, deduped := 0, 0
			for hash, body := range bodies {
				if _, exists := fixture.chunks[hash]; exists {
					deduped++
					continue
				}
				fixture.chunks[hash] = append([]byte(nil), body...)
				stored++
			}
			return stored, deduped, nil
		},
	}
	fixture.sources = &wave58PublicationSources{verify: func(ctx context.Context, repo domain.ContentHash, snapshot domain.Snapshot) error {
		if err := fixture.observe(ctx, repo, "verify-source"); err != nil {
			return err
		}
		fixture.sourceChecks = append(fixture.sourceChecks, snapshot)
		if fixture.sourceErr != nil {
			return fixture.sourceErr
		}
		body, exists := fixture.documents[snapshot.DocHash]
		if !exists {
			return domain.ErrNotFound
		}
		if snapshot.DocIdentity != domain.DocumentIdentityLegacy || domain.HashContent(body) != snapshot.DocHash {
			return domain.ErrIntegrity
		}
		return nil
	}}
	fixture.verified = &wave58PublicationVerified{put: func(ctx context.Context, repo domain.ContentHash, document domain.VerifiedSessionDoc) (bool, error) {
		if err := fixture.observe(ctx, repo, "put-verified"); err != nil {
			return false, err
		}
		fixture.verifiedWrites++
		fixture.proofs = append(fixture.proofs, document)
		if !document.Valid() || document.DocumentRef().Identity != domain.DocumentIdentityLegacy || domain.HashContent(document.Bytes()) != document.Hash() {
			test.Fatal("writer received an invalid verified document")
		}
		if fixture.verifiedErr != nil {
			return false, fixture.verifiedErr
		}
		_, exists := fixture.documents[document.Hash()]
		fixture.documents[document.Hash()] = document.Bytes()
		return !exists, nil
	}}
	fixture.legacy = &wave58PublicationLegacy{put: func(ctx context.Context, repo domain.ContentHash, document domain.SessionDoc) (bool, error) {
		if err := fixture.observe(ctx, repo, "put-legacy"); err != nil {
			return false, err
		}
		fixture.legacyWrites++
		fixture.legacyDocuments = append(fixture.legacyDocuments, document)
		if fixture.legacyErr != nil {
			return false, fixture.legacyErr
		}
		body, err := domain.CanonicalBytes(document.CIR)
		if err != nil {
			return false, err
		}
		_, exists := fixture.documents[document.Hash]
		fixture.documents[document.Hash] = body
		return !exists, nil
	}}
	fixture.config = DocumentPublicationCommandsConfig{
		Profile: DocumentPublicationComplete, Store: fixture.store, Chunks: fixture.chunkStore, Scope: fixture.scope,
		Sources: fixture.sources, Verified: fixture.verified, LegacyWriter: fixture.legacy,
		SupportedIdentities: []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1},
	}
	return fixture
}

func wave58PublicationOwner(test *testing.T, config DocumentPublicationCommandsConfig) *DocumentPublicationCommands {
	test.Helper()
	owner, err := NewDocumentPublicationCommands(config)
	if err != nil || owner == nil {
		test.Fatalf("construct publication owner: owner=%v err=%v", owner, err)
	}
	return owner
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

func wave58PublicationSnapshot(repo domain.ContentHash, document domain.SessionDoc) domain.Snapshot {
	return domain.Snapshot{ID: document.Hash, RepoID: repo, DocHash: document.Hash, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}
}

func (fixture *wave58PublicationFixture) assertNoWrites() {
	fixture.test.Helper()
	if fixture.verifiedWrites != 0 || fixture.legacyWrites != 0 || fixture.chunkWrites != 0 || fixture.snapshotWrites != 0 || fixture.graphChanges != 0 {
		fixture.test.Fatalf("validation wrote objects: verified=%d legacy=%d chunks=%d snapshots=%d graph=%d", fixture.verifiedWrites, fixture.legacyWrites, fixture.chunkWrites, fixture.snapshotWrites, fixture.graphChanges)
	}
}

func TestWave58DocumentPublicationConstructor(test *testing.T) {
	for _, profile := range []DocumentPublicationProfile{DocumentPublicationComplete, DocumentPublicationCompatibility} {
		test.Run(string(profile), func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			fixture.config.Profile = profile
			wave58PublicationOwner(test, fixture.config)
			if len(fixture.events) != 0 || fixture.callbacks != 0 {
				test.Fatal("constructor performed repository work")
			}
		})
	}
	for _, testCase := range []struct {
		name   string
		change func(*DocumentPublicationCommandsConfig)
		want   error
	}{
		{"missing-store", func(config *DocumentPublicationCommandsConfig) { config.Store = nil }, domain.ErrValidation},
		{"missing-chunks", func(config *DocumentPublicationCommandsConfig) { config.Chunks = nil }, domain.ErrValidation},
		{"missing-scope", func(config *DocumentPublicationCommandsConfig) { config.Scope = nil }, domain.ErrValidation},
		{"missing-sources", func(config *DocumentPublicationCommandsConfig) { config.Sources = nil }, domain.ErrCatalogUnsupported},
		{"missing-verified", func(config *DocumentPublicationCommandsConfig) { config.Verified = nil }, domain.ErrCatalogUnsupported},
		{"typed-nil-store", func(config *DocumentPublicationCommandsConfig) { config.Store = (*wave58PublicationStore)(nil) }, domain.ErrValidation},
		{"typed-nil-chunks", func(config *DocumentPublicationCommandsConfig) { config.Chunks = (*wave58PublicationChunks)(nil) }, domain.ErrValidation},
		{"typed-nil-scope", func(config *DocumentPublicationCommandsConfig) { config.Scope = (*wave58PublicationScope)(nil) }, domain.ErrValidation},
		{"typed-nil-sources", func(config *DocumentPublicationCommandsConfig) { config.Sources = (*wave58PublicationSources)(nil) }, domain.ErrValidation},
		{"typed-nil-verified", func(config *DocumentPublicationCommandsConfig) { config.Verified = (*wave58PublicationVerified)(nil) }, domain.ErrValidation},
		{"typed-nil-legacy", func(config *DocumentPublicationCommandsConfig) { config.LegacyWriter = (*wave58PublicationLegacy)(nil) }, domain.ErrValidation},
		{"empty-profile", func(config *DocumentPublicationCommandsConfig) { config.Profile = "" }, domain.ErrValidation},
		{"unknown-profile", func(config *DocumentPublicationCommandsConfig) { config.Profile = "future" }, domain.ErrValidation},
		{"empty-identities", func(config *DocumentPublicationCommandsConfig) { config.SupportedIdentities = nil }, domain.ErrValidation},
		{"missing-legacy", func(config *DocumentPublicationCommandsConfig) {
			config.SupportedIdentities = []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
		}, domain.ErrValidation},
		{"duplicate-identities", func(config *DocumentPublicationCommandsConfig) {
			config.SupportedIdentities = []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityLegacy}
		}, domain.ErrValidation},
		{"unknown-identity", func(config *DocumentPublicationCommandsConfig) {
			config.SupportedIdentities = []domain.DocumentIdentity{domain.DocumentIdentityLegacy, "future"}
		}, domain.ErrValidation},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			testCase.change(&fixture.config)
			owner, err := NewDocumentPublicationCommands(fixture.config)
			if owner != nil || !errors.Is(err, testCase.want) {
				test.Fatalf("constructor: owner=%v err=%v, want %v", owner, err, testCase.want)
			}
			fixture.assertNoWrites()
		})
	}
	fixture := newWave58PublicationFixture(test)
	fixture.config.LegacyWriter = nil
	wave58PublicationOwner(test, fixture.config)
	fixture.config.Profile = DocumentPublicationCompatibility
	fixture.config.Sources, fixture.config.Verified = nil, nil
	wave58PublicationOwner(test, fixture.config)
}

func TestWave58DocumentPublicationConstructorCopiesSupportedIdentities(test *testing.T) {
	fixture := newWave58PublicationFixture(test)
	declared := fixture.config.SupportedIdentities
	owner := wave58PublicationOwner(test, fixture.config)
	declared[0], declared[1] = "mutated-legacy", "mutated-root"
	if !reflect.DeepEqual(owner.supported, []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}) {
		test.Fatalf("supported identities alias caller memory: %v", owner.supported)
	}
	owner.supported[1] = domain.DocumentIdentityLegacy
	if declared[1] != "mutated-root" {
		test.Fatal("owner identity slice aliases configuration")
	}
}

func TestWave58DocumentPublicationCommitTypedBodyAndDeduplication(test *testing.T) {
	fixture := newWave58PublicationFixture(test)
	created := json.Number("1787683260.123456789")
	document := wave58PublicationDocument(test, "typed-body",
		domain.CIREvent{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "exact typed body"}}, ProviderMetadata: &domain.ProviderMetadata{TurnID: "typed-turn", CreateTime: &created}},
		domain.CIREvent{Kind: domain.EventToolCall, Seq: 1, CallID: "typed-call", ToolName: "inspect", Input: map[string]any{"count": json.Number("42"), "ratio": 0.125}},
		domain.CIREvent{Kind: domain.EventCompaction, Seq: 2, ReplacementComplete: true, Replacement: []domain.CIREvent{{Kind: domain.EventToolResult, Seq: 0, CallID: "typed-call", Output: map[string]any{"value": json.Number("1000")}}}},
	)
	canonical := wave58PublicationBytes(test, document)
	input := inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{wave58PublicationSnapshot(fixture.repo, document)}}
	owner := wave58PublicationOwner(test, fixture.config)
	output, err := owner.Commit(fixture.ctx, input)
	if err != nil || output != (inbound.CommitOutput{StoredDocs: 1, StoredSnapshots: 1}) {
		test.Fatalf("typed commit: %+v %v", output, err)
	}
	if len(fixture.proofs) != 1 || !bytes.Equal(fixture.proofs[0].Bytes(), canonical) || fixture.proofs[0].Hash() != document.Hash || !bytes.Contains(canonical, []byte(created)) {
		test.Fatal("verified writer lost the original typed canonical bytes or hash")
	}
	if !bytes.Equal(fixture.documents[document.Hash], canonical) || fixture.legacyWrites != 0 || len(fixture.sourceChecks) != 0 {
		test.Fatal("submitted body took a legacy or stored-source path")
	}
	output, err = owner.Commit(fixture.ctx, input)
	if err != nil || output != (inbound.CommitOutput{DedupedDocs: 1, DedupedSnapshots: 1}) || fixture.graphChanges != 0 {
		test.Fatalf("unchanged replay: %+v %v graph=%d", output, err, fixture.graphChanges)
	}
	if fixture.commitScopes != 2 || fixture.chunkScopes != 0 || fixture.callbacks != 2 || fixture.verifiedWrites != 2 {
		test.Fatal("owner bypassed or nested the document scope")
	}
	firstBody, firstSnapshot := -1, -1
	for position, event := range fixture.events {
		if event == "put-verified" && firstBody < 0 {
			firstBody = position
		}
		if event == "put-snapshot" && firstSnapshot < 0 {
			firstSnapshot = position
		}
	}
	if firstBody < 0 || firstSnapshot <= firstBody {
		test.Fatal("snapshot was written before its verified body")
	}
}

func TestWave58DocumentPublicationValidDuplicateBodiesAreCounted(test *testing.T) {
	fixture := newWave58PublicationFixture(test)
	document := wave58PublicationDocument(test, "duplicate-body")
	output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, inbound.CommitInput{
		RepoID: fixture.repo, Docs: []domain.SessionDoc{document, document}, Snapshots: []domain.Snapshot{wave58PublicationSnapshot(fixture.repo, document)},
	})
	if err != nil || output != (inbound.CommitOutput{StoredDocs: 1, DedupedDocs: 1, StoredSnapshots: 1}) || fixture.verifiedWrites != 2 {
		test.Fatalf("valid duplicate bodies: %+v %v writes=%d", output, err, fixture.verifiedWrites)
	}
}

func TestWave58DocumentPublicationNumericRefusalPrecedesEveryWrite(test *testing.T) {
	for _, location := range []string{"tool-input", "tool-output", "nested-compaction"} {
		for _, layout := range []string{"invalid-first", "invalid-last", "duplicate-claim"} {
			test.Run(location+"/"+layout, func(test *testing.T) {
				fixture := newWave58PublicationFixture(test)
				event := domain.CIREvent{Kind: domain.EventToolCall, Seq: 0, CallID: "numeric-call", ToolName: "numeric", Input: map[string]any{"nested": []any{map[string]any{"count": json.Number("9007199254740993")}}}}
				if location == "tool-output" {
					event = domain.CIREvent{Kind: domain.EventToolResult, Seq: 0, CallID: "numeric-call", Output: []any{map[string]any{"count": json.Number("1E+3")}}}
				}
				if location == "nested-compaction" {
					event = domain.CIREvent{Kind: domain.EventCompaction, Seq: 0, ReplacementComplete: true, Replacement: []domain.CIREvent{{Kind: domain.EventCompaction, Seq: 0, ReplacementComplete: true, Replacement: []domain.CIREvent{{Kind: domain.EventToolResult, Seq: 0, CallID: "nested-call", Output: map[string]any{"count": json.Number("1e999")}}}}}}
				}
				invalid := wave58PublicationDocument(test, "numeric-invalid", event)
				valid := wave58PublicationDocument(test, "numeric-valid-prefix")
				documents := []domain.SessionDoc{invalid, valid}
				if layout != "invalid-first" {
					documents = []domain.SessionDoc{valid, invalid}
				}
				if layout == "duplicate-claim" {
					documents[1].Hash = valid.Hash
				}
				output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, inbound.CommitInput{
					RepoID: fixture.repo, Docs: documents, Snapshots: []domain.Snapshot{wave58PublicationSnapshot(fixture.repo, valid)},
				})
				if !errors.Is(err, domain.ErrIntegrity) || output != (inbound.CommitOutput{}) {
					test.Fatalf("numeric refusal: %+v %v", output, err)
				}
				fixture.assertNoWrites()
			})
		}
	}
}

func TestWave58DocumentPublicationEveryClaimedBodyIsValidated(test *testing.T) {
	for _, layout := range []string{"invalid-first", "invalid-last", "forged-duplicate"} {
		test.Run(layout, func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			valid := wave58PublicationDocument(test, "valid-body")
			invalid := wave58PublicationDocument(test, "different-body")
			invalid.Hash = wave58PublicationHash("false-document-claim")
			documents := []domain.SessionDoc{valid, invalid}
			if layout == "invalid-first" {
				documents = []domain.SessionDoc{invalid, valid}
			}
			if layout == "forged-duplicate" {
				documents[1].Hash = valid.Hash
			}
			output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, inbound.CommitInput{RepoID: fixture.repo, Docs: documents})
			if !errors.Is(err, domain.ErrIntegrity) || output != (inbound.CommitOutput{}) {
				test.Fatalf("body validation: %+v %v", output, err)
			}
			fixture.assertNoWrites()
		})
	}
}

func TestWave58DocumentPublicationMixedUnsupportedIdentitiesStopBeforeReads(test *testing.T) {
	for _, location := range []string{"raw-root", "chunked-root", "unsupported-snapshot-root", "unknown-raw", "unknown-chunked", "unknown-snapshot"} {
		test.Run(location, func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			valid := wave58PublicationDocument(test, "legacy-prefix")
			input := inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{valid}, Snapshots: []domain.Snapshot{wave58PublicationSnapshot(fixture.repo, valid)}}
			identity := domain.DocumentIdentityRootV1
			if location == "unknown-raw" || location == "unknown-chunked" || location == "unknown-snapshot" {
				identity = "future-document-identity"
			}
			switch location {
			case "raw-root", "unknown-raw":
				unsupported := wave58PublicationDocument(test, "unsupported-body")
				unsupported.Identity = identity
				input.Docs = append(input.Docs, unsupported)
			case "chunked-root", "unknown-chunked":
				input.ChunkedDocs = []inbound.ChunkedDoc{{Hash: wave58PublicationHash("root"), Identity: identity, Chunks: []domain.ContentHash{wave58PublicationHash("must-not-read")}}}
			case "unsupported-snapshot-root", "unknown-snapshot":
				fixture.config.SupportedIdentities = []domain.DocumentIdentity{domain.DocumentIdentityLegacy}
				unsupported := wave58PublicationSnapshot(fixture.repo, wave58PublicationDocument(test, "unsupported-snapshot"))
				unsupported.DocIdentity = identity
				input.Snapshots = append(input.Snapshots, unsupported)
			}
			output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, input)
			if !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) || output != (inbound.CommitOutput{}) {
				test.Fatalf("mixed identity batch: %+v %v", output, err)
			}
			fixture.assertNoWrites()
			if len(fixture.events) != 0 || len(fixture.chunkReads) != 0 || len(fixture.sourceChecks) != 0 {
				test.Fatalf("unsupported batch reached a source: %v", fixture.events)
			}
		})
	}
}

func TestWave58DocumentPublicationStoredRootUsesExplicitSources(test *testing.T) {
	fixture := newWave58PublicationFixture(test)
	fixture.metadata.RequiredDocIdentity = domain.DocumentIdentityRootV1
	root := wave58PublicationHash("already-owned-root")
	snapshot := domain.Snapshot{ID: root, RepoID: fixture.repo, DocHash: root, DocIdentity: domain.DocumentIdentityRootV1, Branch: "main"}
	fixture.sources.verify = func(ctx context.Context, repo domain.ContentHash, received domain.Snapshot) error {
		if err := fixture.observe(ctx, repo, "verify-root"); err != nil {
			return err
		}
		if received.ID != root || received.DocHash != root || received.DocIdentity != domain.DocumentIdentityRootV1 || received.RepoID != fixture.repo {
			test.Fatalf("source lost root identity: %+v", received)
		}
		fixture.sourceChecks = append(fixture.sourceChecks, received)
		return nil
	}
	output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, inbound.CommitInput{RepoID: fixture.repo, Snapshots: []domain.Snapshot{snapshot}})
	if err != nil || output != (inbound.CommitOutput{StoredSnapshots: 1}) || len(fixture.sourceChecks) != 1 || fixture.snapshots[root].DocIdentity != domain.DocumentIdentityRootV1 {
		test.Fatalf("stored root metadata: %+v %v sources=%d", output, err, len(fixture.sourceChecks))
	}
	if fixture.verifiedWrites != 0 || fixture.legacyWrites != 0 || fixture.chunkWrites != 0 {
		test.Fatal("metadata-only root was republished through a legacy body path")
	}
}

func TestWave58DocumentPublicationSnapshotValidationPrecedesEveryWrite(test *testing.T) {
	for _, testCase := range []struct {
		name   string
		change func(*domain.Snapshot)
		want   error
	}{
		{"malformed-id", func(snapshot *domain.Snapshot) { snapshot.ID = "invalid" }, domain.ErrIntegrity},
		{"malformed-document", func(snapshot *domain.Snapshot) { snapshot.DocHash = "invalid" }, domain.ErrIntegrity},
		{"id-document-disagree", func(snapshot *domain.Snapshot) { snapshot.ID = wave58PublicationHash("other-id") }, domain.ErrIntegrity},
		{"malformed-memory", func(snapshot *domain.Snapshot) { snapshot.MemoryHash = "invalid" }, domain.ErrIntegrity},
		{"malformed-claude-settings", func(snapshot *domain.Snapshot) { snapshot.ClaudeSettings = "invalid" }, domain.ErrIntegrity},
		{"malformed-agents-settings", func(snapshot *domain.Snapshot) { snapshot.AgentsSettings = "invalid" }, domain.ErrIntegrity},
		{"malformed-codex-settings", func(snapshot *domain.Snapshot) { snapshot.CodexSettings = "invalid" }, domain.ErrIntegrity},
		{"malformed-parent", func(snapshot *domain.Snapshot) { snapshot.Parents = []domain.ContentHash{"invalid"} }, domain.ErrIntegrity},
		{"malformed-graft-parent", func(snapshot *domain.Snapshot) { snapshot.GraftParents = []domain.ContentHash{"invalid"} }, domain.ErrIntegrity},
		{"unknown-identity", func(snapshot *domain.Snapshot) { snapshot.DocIdentity = "future" }, domain.ErrUnsupportedDocumentIdentity},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			first := wave58PublicationDocument(test, "snapshot-valid-prefix")
			last := wave58PublicationDocument(test, "snapshot-invalid-tail")
			invalid := wave58PublicationSnapshot(fixture.repo, last)
			testCase.change(&invalid)
			output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, inbound.CommitInput{
				RepoID: fixture.repo, Docs: []domain.SessionDoc{first, last},
				Snapshots: []domain.Snapshot{wave58PublicationSnapshot(fixture.repo, first), invalid},
			})
			if !errors.Is(err, testCase.want) || output != (inbound.CommitOutput{}) {
				test.Fatalf("snapshot validation: %+v %v, want %v", output, err, testCase.want)
			}
			fixture.assertNoWrites()
		})
	}
}

func TestWave58DocumentPublicationSanitizesMemoryBranchesAndRepository(test *testing.T) {
	fixture := newWave58PublicationFixture(test)
	document := wave58PublicationDocument(test, "sanitize-metadata")
	snapshot := wave58PublicationSnapshot(wave58PublicationHash("client-chosen-repository"), document)
	snapshot.MemoryHash = wave58PublicationHash("client-chosen-memory")
	snapshot.Branches = []string{"forged-membership", "other-branch"}
	input := inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{snapshot}}
	output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, input)
	if err != nil || output != (inbound.CommitOutput{StoredDocs: 1, StoredSnapshots: 1}) {
		test.Fatalf("normalized snapshot: %+v %v", output, err)
	}
	stored := fixture.snapshots[document.Hash]
	if stored.MemoryHash != "" || stored.Branches != nil || stored.RepoID != fixture.repo || stored.Branch != "main" || stored.ID != document.Hash || stored.DocHash != document.Hash {
		test.Fatalf("client-derived metadata escaped sanitization: %+v", stored)
	}
	if !reflect.DeepEqual(input.Snapshots[0], snapshot) {
		test.Fatal("normalization mutated the caller snapshot")
	}
}

func TestWave58DocumentPublicationSettingsPreflight(test *testing.T) {
	for _, kind := range []string{"claude", "agents", "codex"} {
		test.Run("missing-"+kind, func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			document := wave58PublicationDocument(test, "missing-settings")
			snapshot := wave58PublicationSnapshot(fixture.repo, document)
			missing := wave58PublicationHash("missing-settings")
			switch kind {
			case "claude":
				snapshot.ClaudeSettings = missing
			case "agents":
				snapshot.AgentsSettings = missing
			case "codex":
				snapshot.CodexSettings = missing
			}
			output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{snapshot}})
			if !errors.Is(err, domain.ErrIntegrity) || output != (inbound.CommitOutput{}) || !reflect.DeepEqual(fixture.settingsReads, []domain.ContentHash{missing}) {
				test.Fatalf("missing settings: %+v %v reads=%v", output, err, fixture.settingsReads)
			}
			fixture.assertNoWrites()
		})
	}
	test.Run("reuse-validated-settings", func(test *testing.T) {
		fixture := newWave58PublicationFixture(test)
		settings := wave58PublicationHash("shared-settings")
		fixture.settings[settings] = domain.SettingsBundle{Kind: "codex", Files: []domain.SettingsFile{{Path: "config.toml", ContentB64: "dGVzdA=="}}}
		first := wave58PublicationDocument(test, "settings-first")
		last := wave58PublicationDocument(test, "settings-last")
		input := inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{first, last}}
		for _, document := range input.Docs {
			snapshot := wave58PublicationSnapshot(fixture.repo, document)
			snapshot.ClaudeSettings, snapshot.AgentsSettings, snapshot.CodexSettings = settings, settings, settings
			input.Snapshots = append(input.Snapshots, snapshot)
		}
		output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, input)
		if err != nil || output != (inbound.CommitOutput{StoredDocs: 2, StoredSnapshots: 2}) || !reflect.DeepEqual(fixture.settingsReads, []domain.ContentHash{settings}) {
			test.Fatalf("shared settings: %+v %v reads=%v", output, err, fixture.settingsReads)
		}
		for _, snapshot := range fixture.snapshots {
			if snapshot.ClaudeSettings != settings || snapshot.AgentsSettings != settings || snapshot.CodexSettings != settings {
				test.Fatal("settings references were discarded")
			}
		}
	})
}

func TestWave58DocumentPublicationGraphValidationBeforePublication(test *testing.T) {
	for _, scenario := range []string{
		"missing-parent", "self-parent", "duplicate-parent", "duplicate-snapshot", "duplicate-hides-invalid-parent", "incoming-cycle", "existing-graft-cycle",
		"new-grafted-marker", "new-graft-parent", "new-graft-sequence", "rewrite-natural-parents", "extend-existing-grafts", "mark-existing-grafted", "advance-existing-graft-sequence",
	} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			first := wave58PublicationDocument(test, "graph-first")
			second := wave58PublicationDocument(test, "graph-second")
			parent := wave58PublicationDocument(test, "graph-existing-parent")
			firstSnapshot := wave58PublicationSnapshot(fixture.repo, first)
			secondSnapshot := wave58PublicationSnapshot(fixture.repo, second)
			parentSnapshot := wave58PublicationSnapshot(fixture.repo, parent)
			fixture.snapshots[parent.Hash] = parentSnapshot
			input := inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{first, second}, Snapshots: []domain.Snapshot{firstSnapshot, secondSnapshot}}
			switch scenario {
			case "missing-parent":
				input.Snapshots[1].Parents = []domain.ContentHash{wave58PublicationHash("absent-parent")}
			case "self-parent":
				input.Snapshots[1].Parents = []domain.ContentHash{second.Hash}
			case "duplicate-parent":
				input.Snapshots[1].Parents = []domain.ContentHash{parent.Hash, parent.Hash}
			case "duplicate-snapshot":
				input.Snapshots = append(input.Snapshots, secondSnapshot)
			case "duplicate-hides-invalid-parent":
				input.Snapshots[1].Parents = []domain.ContentHash{wave58PublicationHash("absent-parent")}
				input.Snapshots = append(input.Snapshots, secondSnapshot)
			case "incoming-cycle":
				input.Snapshots[0].Parents = []domain.ContentHash{second.Hash}
				input.Snapshots[1].Parents = []domain.ContentHash{first.Hash}
			case "existing-graft-cycle":
				parentSnapshot.Grafted, parentSnapshot.GraftParents, parentSnapshot.GraftSeq = true, []domain.ContentHash{second.Hash}, 3
				fixture.snapshots[parent.Hash] = parentSnapshot
				input.Snapshots[1].Parents = []domain.ContentHash{parent.Hash}
			case "new-grafted-marker":
				input.Snapshots[1].Grafted = true
			case "new-graft-parent":
				input.Snapshots[1].GraftParents = []domain.ContentHash{parent.Hash}
			case "new-graft-sequence":
				input.Snapshots[1].GraftSeq = 1
			case "rewrite-natural-parents":
				fixture.snapshots[second.Hash] = secondSnapshot
				input.Snapshots[1].Parents = []domain.ContentHash{parent.Hash}
			case "extend-existing-grafts":
				secondSnapshot.Grafted, secondSnapshot.GraftParents, secondSnapshot.GraftSeq = true, []domain.ContentHash{parent.Hash}, 4
				fixture.snapshots[second.Hash] = secondSnapshot
				input.Snapshots[1].GraftParents = []domain.ContentHash{first.Hash}
			case "mark-existing-grafted":
				fixture.snapshots[second.Hash] = secondSnapshot
				input.Snapshots[1].Grafted = true
			case "advance-existing-graft-sequence":
				secondSnapshot.GraftSeq = 4
				fixture.snapshots[second.Hash] = secondSnapshot
				input.Snapshots[1].GraftSeq = 5
			}
			output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, input)
			if !errors.Is(err, domain.ErrIntegrity) || output != (inbound.CommitOutput{}) {
				test.Fatalf("graph preflight: %+v %v", output, err)
			}
			fixture.assertNoWrites()
		})
	}
}

func TestWave58DocumentPublicationGraphAcceptsForwardParentsAndServerEcho(test *testing.T) {
	test.Run("forward-parent", func(test *testing.T) {
		fixture := newWave58PublicationFixture(test)
		parent := wave58PublicationDocument(test, "forward-parent")
		child := wave58PublicationDocument(test, "forward-child")
		childSnapshot := wave58PublicationSnapshot(fixture.repo, child)
		childSnapshot.Parents = []domain.ContentHash{parent.Hash}
		output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, inbound.CommitInput{
			RepoID: fixture.repo, Docs: []domain.SessionDoc{child, parent}, Snapshots: []domain.Snapshot{childSnapshot, wave58PublicationSnapshot(fixture.repo, parent)},
		})
		if err != nil || output != (inbound.CommitOutput{StoredDocs: 2, StoredSnapshots: 2}) || !reflect.DeepEqual(fixture.snapshots[child.Hash].Parents, []domain.ContentHash{parent.Hash}) {
			test.Fatalf("forward parent: %+v %v", output, err)
		}
	})
	test.Run("server-graft-echo", func(test *testing.T) {
		fixture := newWave58PublicationFixture(test)
		parent := wave58PublicationDocument(test, "echo-parent")
		document := wave58PublicationDocument(test, "echo-child")
		fixture.snapshots[parent.Hash] = wave58PublicationSnapshot(fixture.repo, parent)
		snapshot := wave58PublicationSnapshot(fixture.repo, document)
		snapshot.Grafted, snapshot.GraftParents, snapshot.GraftSeq = true, []domain.ContentHash{parent.Hash}, 9
		fixture.snapshots[document.Hash] = snapshot
		fixture.documents[document.Hash] = wave58PublicationBytes(test, document)
		output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, inbound.CommitInput{RepoID: fixture.repo, Snapshots: []domain.Snapshot{snapshot}})
		if err != nil || output != (inbound.CommitOutput{DedupedSnapshots: 1}) || !reflect.DeepEqual(fixture.snapshots[document.Hash], snapshot) || fixture.graphChanges != 0 {
			test.Fatalf("server graft echo: %+v %v graph=%d", output, err, fixture.graphChanges)
		}
		if len(fixture.sourceChecks) != 1 || fixture.verifiedWrites != 0 || fixture.legacyWrites != 0 {
			test.Fatal("metadata-only echo did not use the current document source")
		}
	})
}

func TestWave58DocumentPublicationChangedDuplicateUsesBoundGraphScope(test *testing.T) {
	for _, failure := range []bool{false, true} {
		test.Run(map[bool]string{false: "success", true: "graph-failure"}[failure], func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			document := wave58PublicationDocument(test, "changed-existing-metadata")
			snapshot := wave58PublicationSnapshot(fixture.repo, document)
			snapshot.Branch = domain.StashBranchLabel
			fixture.snapshots[document.Hash] = snapshot
			fixture.documents[document.Hash] = wave58PublicationBytes(test, document)
			snapshot.Branch = "main"
			snapshot.Message = "new out-of-hash metadata"
			want := errors.New("graph revision failed")
			if failure {
				fixture.graphErr = want
			}
			output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, inbound.CommitInput{RepoID: fixture.repo, Snapshots: []domain.Snapshot{snapshot}})
			if failure {
				if !errors.Is(err, want) || output != (inbound.CommitOutput{}) {
					test.Fatalf("graph failure receipt: %+v %v", output, err)
				}
			} else if err != nil || output != (inbound.CommitOutput{DedupedSnapshots: 1}) {
				test.Fatalf("changed duplicate: %+v %v", output, err)
			}
			if fixture.graphChanges != 1 || fixture.commitScopes != 1 || fixture.callbacks != 1 || fixture.snapshotWrites != 1 {
				test.Fatal("graph change bypassed or retried the original bound scope")
			}
		})
	}
}

func TestWave58DocumentPublicationStrongSourceErrorsAreTerminal(test *testing.T) {
	storageFailure := errors.New("current source unavailable")
	for _, testCase := range []struct {
		name    string
		failure error
		want    error
	}{
		{"not-found", domain.ErrNotFound, domain.ErrIntegrity},
		{"integrity", domain.ErrIntegrity, domain.ErrIntegrity},
		{"unsupported", domain.ErrCatalogUnsupported, domain.ErrCatalogUnsupported},
		{"storage", storageFailure, storageFailure},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			owned := wave58PublicationDocument(test, "owned-but-current-verification-fails")
			prefix := wave58PublicationDocument(test, "must-not-publish-prefix")
			fixture.documents[owned.Hash] = wave58PublicationBytes(test, owned)
			fixture.sourceErr = testCase.failure
			output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, inbound.CommitInput{
				RepoID: fixture.repo, Docs: []domain.SessionDoc{prefix},
				Snapshots: []domain.Snapshot{wave58PublicationSnapshot(fixture.repo, prefix), wave58PublicationSnapshot(fixture.repo, owned)},
			})
			if !errors.Is(err, testCase.want) || output != (inbound.CommitOutput{}) || len(fixture.sourceChecks) != 1 {
				test.Fatalf("strong source error: %+v %v checks=%d", output, err, len(fixture.sourceChecks))
			}
			fixture.assertNoWrites()
		})
	}
}

func TestWave58DocumentPublicationRechecksCurrentSourceOnReplay(test *testing.T) {
	fixture := newWave58PublicationFixture(test)
	document := wave58PublicationDocument(test, "source-replay")
	fixture.documents[document.Hash] = wave58PublicationBytes(test, document)
	input := inbound.CommitInput{RepoID: fixture.repo, Snapshots: []domain.Snapshot{wave58PublicationSnapshot(fixture.repo, document)}}
	owner := wave58PublicationOwner(test, fixture.config)
	if output, err := owner.Commit(fixture.ctx, input); err != nil || output != (inbound.CommitOutput{StoredSnapshots: 1}) {
		test.Fatalf("initial current source: %+v %v", output, err)
	}
	fixture.sourceErr = domain.ErrIntegrity
	output, err := owner.Commit(fixture.ctx, input)
	if !errors.Is(err, domain.ErrIntegrity) || output != (inbound.CommitOutput{}) || len(fixture.sourceChecks) != 2 || fixture.snapshotWrites != 1 {
		test.Fatalf("stale source success was reused: %+v %v checks=%d writes=%d", output, err, len(fixture.sourceChecks), fixture.snapshotWrites)
	}
	if fixture.legacyWrites != 0 || fixture.verifiedWrites != 0 {
		test.Fatal("source replay fell back to a document writer")
	}
}

func TestWave58DocumentPublicationVerifiedWriterNeverFallsBack(test *testing.T) {
	for _, failure := range []error{domain.ErrCatalogUnsupported, domain.ErrIntegrity, errors.New("verified publication failed")} {
		test.Run(failure.Error(), func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			document := wave58PublicationDocument(test, "verified-writer-failure")
			fixture.verifiedErr = failure
			output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, inbound.CommitInput{
				RepoID: fixture.repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{wave58PublicationSnapshot(fixture.repo, document)},
			})
			if !errors.Is(err, failure) || output != (inbound.CommitOutput{}) || fixture.verifiedWrites != 1 || fixture.legacyWrites != 0 || fixture.snapshotWrites != 0 || fixture.commitScopes != 1 {
				test.Fatalf("verified writer fallback/retry: %+v %v verified=%d legacy=%d snapshots=%d", output, err, fixture.verifiedWrites, fixture.legacyWrites, fixture.snapshotWrites)
			}
		})
	}
	test.Run("late-writer-failure-has-no-partial-receipt", func(test *testing.T) {
		fixture := newWave58PublicationFixture(test)
		first := wave58PublicationDocument(test, "writer-valid-prefix")
		last := wave58PublicationDocument(test, "writer-failing-tail")
		failure := errors.New("second verified write failed")
		original := fixture.verified.put
		fixture.verified.put = func(ctx context.Context, repo domain.ContentHash, document domain.VerifiedSessionDoc) (bool, error) {
			if fixture.verifiedWrites == 1 {
				fixture.verifiedErr = failure
			}
			return original(ctx, repo, document)
		}
		output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{first, last}})
		if !errors.Is(err, failure) || output != (inbound.CommitOutput{}) || fixture.verifiedWrites != 2 || fixture.legacyWrites != 0 || fixture.snapshotWrites != 0 {
			test.Fatalf("late writer failure: %+v %v", output, err)
		}
	})
}

func TestWave58DocumentPublicationExplicitLegacyCompatibility(test *testing.T) {
	test.Run("legacy-writer", func(test *testing.T) {
		fixture := newWave58PublicationFixture(test)
		fixture.config.Profile = DocumentPublicationCompatibility
		fixture.config.Verified, fixture.config.Sources = nil, nil
		document := wave58PublicationDocument(test, "explicit-legacy")
		input := inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{wave58PublicationSnapshot(fixture.repo, document)}}
		owner := wave58PublicationOwner(test, fixture.config)
		output, err := owner.Commit(fixture.ctx, input)
		if err != nil || output != (inbound.CommitOutput{StoredDocs: 1, StoredSnapshots: 1}) || fixture.legacyWrites != 1 || fixture.verifiedWrites != 0 {
			test.Fatalf("explicit legacy commit: %+v %v legacy=%d", output, err, fixture.legacyWrites)
		}
		if len(fixture.legacyDocuments) != 1 || !reflect.DeepEqual(fixture.legacyDocuments[0], document) || !bytes.Equal(fixture.documents[document.Hash], wave58PublicationBytes(test, document)) {
			test.Fatal("legacy compatibility changed the validated body")
		}
		output, err = owner.Commit(fixture.ctx, input)
		if err != nil || output != (inbound.CommitOutput{DedupedDocs: 1, DedupedSnapshots: 1}) {
			test.Fatalf("legacy replay: %+v %v", output, err)
		}
	})
	for _, missing := range []string{"writer", "source"} {
		test.Run("missing-"+missing, func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			fixture.config.Profile = DocumentPublicationCompatibility
			fixture.config.Verified, fixture.config.LegacyWriter, fixture.config.Sources = nil, nil, nil
			document := wave58PublicationDocument(test, "absent-compatibility-capability")
			input := inbound.CommitInput{RepoID: fixture.repo}
			if missing == "writer" {
				input.Docs = []domain.SessionDoc{document}
			} else {
				fixture.documents[document.Hash] = wave58PublicationBytes(test, document)
				fixture.snapshots[document.Hash] = wave58PublicationSnapshot(fixture.repo, document)
				input.Snapshots = []domain.Snapshot{fixture.snapshots[document.Hash]}
			}
			output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, input)
			if !errors.Is(err, domain.ErrCatalogUnsupported) || output != (inbound.CommitOutput{}) {
				test.Fatalf("missing compatibility capability: %+v %v", output, err)
			}
			fixture.assertNoWrites()
		})
	}
	test.Run("legacy-writer-failure", func(test *testing.T) {
		fixture := newWave58PublicationFixture(test)
		fixture.config.Profile, fixture.config.Verified = DocumentPublicationCompatibility, nil
		failure := errors.New("legacy publication failed")
		fixture.legacyErr = failure
		document := wave58PublicationDocument(test, "legacy-failure")
		output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}})
		if !errors.Is(err, failure) || output != (inbound.CommitOutput{}) || fixture.legacyWrites != 1 || fixture.verifiedWrites != 0 {
			test.Fatalf("legacy writer failure: %+v %v", output, err)
		}
	})
	for _, optional := range []string{"sources", "verified", "legacy"} {
		test.Run("typed-nil-compatibility-"+optional, func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			fixture.config.Profile = DocumentPublicationCompatibility
			switch optional {
			case "sources":
				fixture.config.Sources = (*wave58PublicationSources)(nil)
			case "verified":
				fixture.config.Verified = (*wave58PublicationVerified)(nil)
			case "legacy":
				fixture.config.LegacyWriter = (*wave58PublicationLegacy)(nil)
			}
			owner, err := NewDocumentPublicationCommands(fixture.config)
			if owner != nil || !errors.Is(err, domain.ErrValidation) {
				test.Fatalf("typed-nil compatibility: owner=%v err=%v", owner, err)
			}
		})
	}
}

func wave58PublicationChunkInput(test *testing.T, repo domain.ContentHash, format string) (inbound.CommitInput, []byte) {
	test.Helper()
	document := wave58PublicationDocument(test, "chunked-"+format,
		domain.CIREvent{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "한글 and escaped \"text\"\nfirst"}}},
		domain.CIREvent{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: "second message and exact ordering"}}},
	)
	canonical := wave58PublicationBytes(test, document)
	var fragments struct {
		Envelope json.RawMessage   `json:"envelope"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(canonical, &fragments); err != nil {
		test.Fatal(err)
	}
	bodies := make([][]byte, 0, len(fragments.Events))
	for _, event := range fragments.Events {
		bodies = append(bodies, []byte(event))
	}
	if format == domain.ChunkFormatV2 {
		stream := bytes.Join(bodies, []byte(","))
		boundary := bytes.Index(stream, []byte("한글")) + 1
		if boundary <= 0 || boundary >= len(stream) {
			test.Fatal("v2 fixture lacks a split inside a UTF-8 event")
		}
		bodies = [][]byte{stream[:boundary], stream[boundary:]}
	}
	chunked := inbound.ChunkedDoc{Hash: document.Hash, Format: format, Envelope: fragments.Envelope}
	input := inbound.CommitInput{RepoID: repo, Snapshots: []domain.Snapshot{wave58PublicationSnapshot(repo, document)}}
	for _, body := range bodies {
		chunk := inbound.ChunkObject{Hash: domain.HashContent(body), Data: append([]byte(nil), body...)}
		chunked.Chunks = append(chunked.Chunks, chunk.Hash)
		input.ChunkObjects = append(input.ChunkObjects, chunk)
	}
	input.ChunkedDocs = []inbound.ChunkedDoc{chunked}
	return input, canonical
}

func TestWave58DocumentPublicationChunkedExactAssemblyAndStaging(test *testing.T) {
	for _, format := range []string{"", domain.ChunkFormatV1, domain.ChunkFormatV2} {
		for _, placement := range []string{"inline", "mixed", "staged"} {
			test.Run(format+"/"+placement, func(test *testing.T) {
				fixture := newWave58PublicationFixture(test)
				input, canonical := wave58PublicationChunkInput(test, fixture.repo, format)
				owner := wave58PublicationOwner(test, fixture.config)
				var staged []inbound.ChunkObject
				if placement == "mixed" {
					staged, input.ChunkObjects = input.ChunkObjects[1:], input.ChunkObjects[:1]
				}
				if placement == "staged" {
					staged, input.ChunkObjects = input.ChunkObjects, nil
				}
				if len(staged) > 0 {
					output, err := owner.StoreChunks(fixture.ctx, inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: staged})
					if err != nil || output != (inbound.StoreChunksOutput{Stored: len(staged)}) {
						test.Fatalf("actual chunk staging: %+v %v", output, err)
					}
				}
				output, err := owner.Commit(fixture.ctx, input)
				if err != nil || output != (inbound.CommitOutput{StoredDocs: 1, StoredSnapshots: 1}) {
					test.Fatalf("chunked commit: %+v %v", output, err)
				}
				hash := input.ChunkedDocs[0].Hash
				if len(fixture.proofs) != 1 || fixture.proofs[0].Hash() != hash || !bytes.Equal(fixture.proofs[0].Bytes(), canonical) || !bytes.Equal(fixture.documents[hash], canonical) {
					test.Fatal("chunk assembly did not preserve exact canonical bytes and identity")
				}
				var wantReads []domain.ContentHash
				for _, chunk := range staged {
					wantReads = append(wantReads, chunk.Hash)
					if !bytes.Equal(fixture.chunks[chunk.Hash], chunk.Data) {
						test.Fatal("staged chunk bytes were not actually stored")
					}
				}
				if !reflect.DeepEqual(fixture.chunkReads, wantReads) || fixture.legacyWrites != 0 || fixture.commitScopes != 1 {
					test.Fatalf("chunk source order/path: reads=%v want=%v", fixture.chunkReads, wantReads)
				}
			})
		}
	}
}

func TestWave58DocumentPublicationChunkFailuresPrecedeAllWrites(test *testing.T) {
	for _, scenario := range []string{"inline-hash", "inline-malformed-hash", "document-hash", "manifest-malformed-hash", "chunk-malformed-hash", "missing-stored-chunk", "corrupt-stored-chunk", "wrong-order", "bad-envelope", "unsupported-format", "empty-manifest"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			input, _ := wave58PublicationChunkInput(test, fixture.repo, domain.ChunkFormatV2)
			prefix := wave58PublicationDocument(test, "valid-prefix-before-bad-chunks")
			input.Docs = []domain.SessionDoc{prefix}
			input.Snapshots = append([]domain.Snapshot{wave58PublicationSnapshot(fixture.repo, prefix)}, input.Snapshots...)
			want := domain.ErrIntegrity
			switch scenario {
			case "inline-hash":
				input.ChunkObjects[1].Data = []byte("wrong current bytes")
			case "inline-malformed-hash":
				input.ChunkObjects[1].Hash = "invalid"
			case "document-hash":
				input.ChunkedDocs[0].Hash = wave58PublicationHash("false-assembled-document")
			case "manifest-malformed-hash":
				input.ChunkedDocs[0].Hash = "invalid"
			case "chunk-malformed-hash":
				input.ChunkedDocs[0].Chunks[1] = "invalid"
			case "missing-stored-chunk":
				input.ChunkObjects = input.ChunkObjects[:1]
			case "corrupt-stored-chunk":
				fixture.chunks[input.ChunkObjects[1].Hash] = []byte("corrupt stored bytes")
				input.ChunkObjects = input.ChunkObjects[:1]
			case "wrong-order":
				input.ChunkedDocs[0].Chunks[0], input.ChunkedDocs[0].Chunks[1] = input.ChunkedDocs[0].Chunks[1], input.ChunkedDocs[0].Chunks[0]
			case "bad-envelope":
				input.ChunkedDocs[0].Envelope = json.RawMessage(`{"cir_version":"wrong-envelope"}`)
			case "unsupported-format":
				input.ChunkedDocs[0].Format = "future-chunk-format"
				want = domain.ErrValidation
			case "empty-manifest":
				input.ChunkedDocs[0].Chunks = nil
			}
			output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, input)
			if !errors.Is(err, want) || output != (inbound.CommitOutput{}) {
				test.Fatalf("chunk validation: %+v %v, want %v", output, err, want)
			}
			fixture.assertNoWrites()
		})
	}
}

func TestWave58DocumentPublicationChunkedNumericRefusalPrecedesWrites(test *testing.T) {
	fixture := newWave58PublicationFixture(test)
	document := wave58PublicationDocument(test, "chunked-numeric-refusal", domain.CIREvent{
		Kind: domain.EventToolCall, Seq: 0, CallID: "numeric", ToolName: "numeric", Input: map[string]any{"count": json.Number("9007199254740993")},
	})
	canonical := wave58PublicationBytes(test, document)
	var fragments struct {
		Envelope json.RawMessage   `json:"envelope"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(canonical, &fragments); err != nil {
		test.Fatal(err)
	}
	body := []byte(fragments.Events[0])
	chunk := inbound.ChunkObject{Hash: domain.HashContent(body), Data: body}
	prefix := wave58PublicationDocument(test, "chunked-numeric-valid-prefix")
	output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, inbound.CommitInput{
		RepoID: fixture.repo, Docs: []domain.SessionDoc{prefix}, ChunkObjects: []inbound.ChunkObject{chunk},
		ChunkedDocs: []inbound.ChunkedDoc{{Hash: document.Hash, Format: domain.ChunkFormatV2, Envelope: fragments.Envelope, Chunks: []domain.ContentHash{chunk.Hash}}},
		Snapshots:   []domain.Snapshot{wave58PublicationSnapshot(fixture.repo, prefix), wave58PublicationSnapshot(fixture.repo, document)},
	})
	if !errors.Is(err, domain.ErrIntegrity) || output != (inbound.CommitOutput{}) {
		test.Fatalf("chunked numeric refusal: %+v %v", output, err)
	}
	fixture.assertNoWrites()
}

func wave58PublicationWireChunk(body []byte) inbound.ChunkObject {
	return inbound.ChunkObject{Hash: domain.HashContent(body), Data: body}
}

func TestWave58DocumentPublicationStoreChunksValidation(test *testing.T) {
	for _, scenario := range []string{"empty", "too-many", "empty-body", "oversized-body", "oversized-total", "malformed-hash", "hash-mismatch", "conflicting-duplicate", "invalid-repository", "unbound-repository"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			valid := wave58PublicationWireChunk([]byte("valid chunk prefix must not be stored"))
			input := inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: []inbound.ChunkObject{valid}}
			want := domain.ErrValidation
			switch scenario {
			case "empty":
				input.Chunks = nil
			case "too-many":
				input.Chunks = make([]inbound.ChunkObject, inbound.MaxChunkWireObjects+1)
				for position := range input.Chunks {
					input.Chunks[position] = valid
				}
			case "empty-body":
				input.Chunks = append(input.Chunks, wave58PublicationWireChunk(nil))
			case "oversized-body":
				input.Chunks = append(input.Chunks, wave58PublicationWireChunk(bytes.Repeat([]byte("large"), inbound.MaxChunkWireRawBytes/5+1)))
			case "oversized-total":
				input.Chunks = []inbound.ChunkObject{
					wave58PublicationWireChunk(bytes.Repeat([]byte("first"), inbound.MaxChunkWireRawBytes/10+1)),
					wave58PublicationWireChunk(bytes.Repeat([]byte("other"), inbound.MaxChunkWireRawBytes/10+1)),
				}
			case "malformed-hash":
				input.Chunks = append(input.Chunks, inbound.ChunkObject{Hash: "invalid", Data: []byte("data")})
				want = domain.ErrIntegrity
			case "hash-mismatch":
				input.Chunks = append(input.Chunks, inbound.ChunkObject{Hash: wave58PublicationHash("false-chunk-claim"), Data: []byte("different bytes")})
				want = domain.ErrIntegrity
			case "conflicting-duplicate":
				input.Chunks = append(input.Chunks, inbound.ChunkObject{Hash: valid.Hash, Data: []byte("disagreeing duplicate body")})
				want = domain.ErrIntegrity
			case "invalid-repository":
				fixture.repo, input.RepoID = "invalid", "invalid"
				want = domain.ErrIntegrity
			case "unbound-repository":
				fixture.metadata.RepositoryID = ""
				want = domain.ErrForbidden
			}
			output, err := wave58PublicationOwner(test, fixture.config).StoreChunks(fixture.ctx, input)
			if !errors.Is(err, want) || output != (inbound.StoreChunksOutput{}) {
				test.Fatalf("chunk store validation: %+v %v, want %v", output, err, want)
			}
			fixture.assertNoWrites()
			if len(fixture.chunks) != 0 {
				test.Fatal("invalid batch stored its valid chunk prefix")
			}
		})
	}
}

func TestWave58DocumentPublicationStoreChunksPersistsDeduplicatesAndHonorsBounds(test *testing.T) {
	test.Run("actual-storage-and-duplicate-counts", func(test *testing.T) {
		fixture := newWave58PublicationFixture(test)
		first := wave58PublicationWireChunk([]byte("stored first body"))
		second := wave58PublicationWireChunk([]byte("stored second body"))
		input := inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: []inbound.ChunkObject{first, second, first}}
		owner := wave58PublicationOwner(test, fixture.config)
		output, err := owner.StoreChunks(fixture.ctx, input)
		if err != nil || output != (inbound.StoreChunksOutput{Stored: 2}) || fixture.chunkWrites != 1 || len(fixture.chunks) != 2 {
			test.Fatalf("actual chunk storage: %+v %v writes=%d chunks=%d", output, err, fixture.chunkWrites, len(fixture.chunks))
		}
		for _, chunk := range []inbound.ChunkObject{first, second} {
			if !bytes.Equal(fixture.chunks[chunk.Hash], chunk.Data) || domain.HashContent(fixture.chunks[chunk.Hash]) != chunk.Hash {
				test.Fatal("chunk store received the wrong body or content key")
			}
		}
		output, err = owner.StoreChunks(fixture.ctx, input)
		if err != nil || output != (inbound.StoreChunksOutput{Deduped: 2}) || fixture.chunkWrites != 2 || fixture.chunkScopes != 2 || fixture.commitScopes != 0 {
			test.Fatalf("chunk replay: %+v %v", output, err)
		}
		if fixture.verifiedWrites != 0 || fixture.legacyWrites != 0 || fixture.snapshotWrites != 0 || fixture.graphChanges != 0 {
			test.Fatal("chunk staging published documents or graph metadata")
		}
	})
	test.Run("exact-byte-limit-and-duplicate-budget", func(test *testing.T) {
		fixture := newWave58PublicationFixture(test)
		chunk := wave58PublicationWireChunk(bytes.Repeat([]byte{'x'}, inbound.MaxChunkWireRawBytes))
		output, err := wave58PublicationOwner(test, fixture.config).StoreChunks(fixture.ctx, inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: []inbound.ChunkObject{chunk, chunk}})
		if err != nil || output != (inbound.StoreChunksOutput{Stored: 1}) || len(fixture.chunks[chunk.Hash]) != inbound.MaxChunkWireRawBytes {
			test.Fatalf("exact raw bound with identical duplicate: %+v %v", output, err)
		}
	})
	test.Run("exact-count-limit", func(test *testing.T) {
		fixture := newWave58PublicationFixture(test)
		var chunks []inbound.ChunkObject
		for position := 0; position < inbound.MaxChunkWireObjects; position++ {
			chunks = append(chunks, wave58PublicationWireChunk([]byte{byte(position), 'x'}))
		}
		output, err := wave58PublicationOwner(test, fixture.config).StoreChunks(fixture.ctx, inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: chunks})
		if err != nil || output != (inbound.StoreChunksOutput{Stored: inbound.MaxChunkWireObjects}) || len(fixture.chunks) != inbound.MaxChunkWireObjects {
			test.Fatalf("exact count bound: %+v %v", output, err)
		}
	})
	test.Run("compatibility-without-document-capabilities", func(test *testing.T) {
		fixture := newWave58PublicationFixture(test)
		fixture.config.Profile = DocumentPublicationCompatibility
		fixture.config.Sources, fixture.config.Verified, fixture.config.LegacyWriter = nil, nil, nil
		chunk := wave58PublicationWireChunk([]byte("staging does not require a document writer"))
		output, err := wave58PublicationOwner(test, fixture.config).StoreChunks(fixture.ctx, inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: []inbound.ChunkObject{chunk}})
		if err != nil || output != (inbound.StoreChunksOutput{Stored: 1}) || !bytes.Equal(fixture.chunks[chunk.Hash], chunk.Data) {
			test.Fatalf("explicit staging compatibility: %+v %v", output, err)
		}
	})
}

func TestWave58DocumentPublicationChunkWriterFailureHasNoReceiptOrRetry(test *testing.T) {
	fixture := newWave58PublicationFixture(test)
	failure := errors.New("chunk store failed after reporting counts")
	fixture.chunkWriteErr = failure
	chunk := wave58PublicationWireChunk([]byte("failed chunk publication"))
	output, err := wave58PublicationOwner(test, fixture.config).StoreChunks(fixture.ctx, inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: []inbound.ChunkObject{chunk}})
	if !errors.Is(err, failure) || output != (inbound.StoreChunksOutput{}) || fixture.chunkWrites != 1 || fixture.chunkScopes != 1 || fixture.callbacks != 1 || len(fixture.chunks) != 0 {
		test.Fatalf("chunk writer failure leaked counts or retried: %+v %v writes=%d scopes=%d", output, err, fixture.chunkWrites, fixture.chunkScopes)
	}
}

func TestDocumentPublicationCompatibilityPreservesLegacyChunkFailureCounts(test *testing.T) {
	fixture := newWave58PublicationFixture(test)
	fixture.config.Profile = DocumentPublicationCompatibility
	failure := errors.New("legacy partial chunk staging failure")
	fixture.chunkWriteErr = failure
	chunk := wave58PublicationWireChunk([]byte("legacy failure counts"))
	output, err := wave58PublicationOwner(test, fixture.config).StoreChunks(fixture.ctx, inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: []inbound.ChunkObject{chunk}})
	if !errors.Is(err, failure) || output != (inbound.StoreChunksOutput{Stored: 9, Deduped: 7}) {
		test.Fatalf("legacy partial staging diagnostics changed: %+v %v", output, err)
	}
}

func TestWave58DocumentPublicationScopeFailuresSuppressReceipts(test *testing.T) {
	for _, operation := range []string{"commit", "chunks"} {
		for _, stage := range []string{"admission", "scope-exit"} {
			test.Run(operation+"/"+stage, func(test *testing.T) {
				fixture := newWave58PublicationFixture(test)
				failure := errors.New("repository scope failed")
				if stage == "admission" {
					fixture.entryErr = failure
					originalCommit, originalChunks := fixture.scope.commit, fixture.scope.chunks
					fixture.scope.commit = func(ctx context.Context, repo domain.ContentHash, callback func(context.Context) (inbound.CommitOutput, error)) (inbound.CommitOutput, error) {
						_, err := originalCommit(ctx, repo, callback)
						return inbound.CommitOutput{StoredDocs: 31, StoredSnapshots: 19}, err
					}
					fixture.scope.chunks = func(ctx context.Context, repo domain.ContentHash, callback func(context.Context) (inbound.StoreChunksOutput, error)) (inbound.StoreChunksOutput, error) {
						_, err := originalChunks(ctx, repo, callback)
						return inbound.StoreChunksOutput{Stored: 31, Deduped: 19}, err
					}
				} else {
					fixture.exitErr = failure
				}
				owner := wave58PublicationOwner(test, fixture.config)
				if operation == "commit" {
					document := wave58PublicationDocument(test, "scope-failure-document")
					output, err := owner.Commit(fixture.ctx, inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{wave58PublicationSnapshot(fixture.repo, document)}})
					if !errors.Is(err, failure) || output != (inbound.CommitOutput{}) || fixture.commitScopes != 1 || fixture.chunkScopes != 0 {
						test.Fatalf("document scope failure: %+v %v scopes=%d", output, err, fixture.commitScopes)
					}
				} else {
					chunk := wave58PublicationWireChunk([]byte("scope-failure-chunk"))
					output, err := owner.StoreChunks(fixture.ctx, inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: []inbound.ChunkObject{chunk}})
					if !errors.Is(err, failure) || output != (inbound.StoreChunksOutput{}) || fixture.chunkScopes != 1 || fixture.commitScopes != 0 {
						test.Fatalf("chunk scope failure: %+v %v scopes=%d", output, err, fixture.chunkScopes)
					}
				}
				wantCallbacks := 1
				if stage == "admission" {
					wantCallbacks = 0
					fixture.assertNoWrites()
				}
				if fixture.callbacks != wantCallbacks || fixture.active {
					test.Fatal("owner retried or retained a failed repository scope")
				}
			})
		}
	}
}

func TestWave58DocumentPublicationCancellationUsesBoundContext(test *testing.T) {
	for _, operation := range []string{"commit", "chunks"} {
		for _, stage := range []string{"already-canceled", "inside-scope"} {
			test.Run(operation+"/"+stage, func(test *testing.T) {
				fixture := newWave58PublicationFixture(test)
				ctx, cancel := context.WithCancel(fixture.ctx)
				defer cancel()
				fixture.ctx = ctx
				if stage == "already-canceled" {
					cancel()
				} else {
					fixture.beforeCallback = cancel
				}
				owner := wave58PublicationOwner(test, fixture.config)
				if operation == "commit" {
					document := wave58PublicationDocument(test, "canceled-document")
					output, err := owner.Commit(fixture.ctx, inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}})
					if !errors.Is(err, context.Canceled) || output != (inbound.CommitOutput{}) || fixture.commitScopes != 1 {
						test.Fatalf("canceled commit: %+v %v", output, err)
					}
				} else {
					chunk := wave58PublicationWireChunk([]byte("canceled-chunk"))
					output, err := owner.StoreChunks(fixture.ctx, inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: []inbound.ChunkObject{chunk}})
					if !errors.Is(err, context.Canceled) || output != (inbound.StoreChunksOutput{}) || fixture.chunkScopes != 1 {
						test.Fatalf("canceled chunk store: %+v %v", output, err)
					}
				}
				fixture.assertNoWrites()
			})
		}
	}
	for _, failedRead := range []bool{false, true} {
		test.Run(map[bool]string{false: "cancel-after-successful-chunk-read", true: "cancel-after-failed-chunk-read"}[failedRead], func(test *testing.T) {
			fixture := newWave58PublicationFixture(test)
			input, _ := wave58PublicationChunkInput(test, fixture.repo, domain.ChunkFormatV2)
			for _, chunk := range input.ChunkObjects {
				fixture.chunks[chunk.Hash] = chunk.Data
			}
			input.ChunkObjects = nil
			ctx, cancel := context.WithCancel(fixture.ctx)
			defer cancel()
			fixture.ctx, fixture.afterChunkRead = ctx, cancel
			if failedRead {
				fixture.chunkReadErr = domain.ErrNotFound
			}
			output, err := wave58PublicationOwner(test, fixture.config).Commit(fixture.ctx, input)
			if !errors.Is(err, context.Canceled) || output != (inbound.CommitOutput{}) || len(fixture.chunkReads) != 1 {
				test.Fatalf("chunk-read cancellation: %+v %v reads=%v", output, err, fixture.chunkReads)
			}
			fixture.assertNoWrites()
		})
	}
}
