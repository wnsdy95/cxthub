package documentquery_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type rootFixtureScope struct{}

func (rootFixtureScope) AuthorizeDocumentRead(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, ctx.Err()
}

func (rootFixtureScope) AuthorizeDocumentPreparation(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, ctx.Err()
}

func (rootFixtureScope) WithinDocumentRead(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
	return read(ctx)
}

func (rootFixtureScope) WithinDocumentSnapshot(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
	return read(ctx)
}

type rootFixtureProjectionCall struct {
	hash       domain.ContentHash
	hashesOnly bool
}

type rootFixtureReader struct {
	*store.FSStore
	snapshotFailures       map[domain.ContentHash]error
	projectionReplacements map[domain.ContentHash]domain.ContentHash
	projectionFailures     map[domain.ContentHash]error
	snapshotCalls          []domain.ContentHash
	projectionCalls        []rootFixtureProjectionCall
	verifiedCalls          []domain.ContentHash
	fallbackCalls          []string
}

func (reader *rootFixtureReader) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	reader.snapshotCalls = append(reader.snapshotCalls, hash)
	snapshot, err := reader.FSStore.GetSnapshot(ctx, repo, hash)
	if err != nil {
		return snapshot, err
	}
	return snapshot, reader.snapshotFailures[hash]
}

func (reader *rootFixtureReader) ReadVerifiedRootEvents(ctx context.Context, repo, hash domain.ContentHash, selection domain.RootEventSelection) (domain.VerifiedRootEvents, error) {
	reader.projectionCalls = append(reader.projectionCalls, rootFixtureProjectionCall{hash: hash, hashesOnly: selection.HashesOnly})
	requested := hash
	if replacement, exists := reader.projectionReplacements[hash]; exists {
		if replacement == "" {
			return domain.VerifiedRootEvents{}, nil
		}
		hash = replacement
	}
	projection, err := reader.FSStore.ReadVerifiedRootEvents(ctx, repo, hash, selection)
	if err != nil {
		return projection, err
	}
	return projection, reader.projectionFailures[requested]
}

func (reader *rootFixtureReader) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	reader.verifiedCalls = append(reader.verifiedCalls, hash)
	return reader.FSStore.ReadVerifiedDoc(ctx, repo, hash)
}

func (reader *rootFixtureReader) rejectFallback(method string) error {
	reader.fallbackCalls = append(reader.fallbackCalls, method)
	return fmt.Errorf("root query attempted legacy fallback: %s", method)
}

func (reader *rootFixtureReader) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	return domain.SessionDoc{}, reader.rejectFallback("GetDoc")
}

func (reader *rootFixtureReader) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	return domain.DocChunkManifest{}, reader.rejectFallback("GetDocManifest")
}

func (reader *rootFixtureReader) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	return nil, reader.rejectFallback("GetChunk")
}

func (reader *rootFixtureReader) DocReadIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocReadIndex, error) {
	return domain.DocReadIndex{}, reader.rejectFallback("DocReadIndex")
}

func (reader *rootFixtureReader) SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error) {
	return nil, reader.rejectFallback("SearchDocEvents")
}

type rootFixtureState struct {
	ctx     context.Context
	repo    domain.ContentHash
	dir     string
	storage *store.FSStore
	reader  *rootFixtureReader
	queries *documentquery.Queries
	target  domain.SessionDoc
	base    domain.SessionDoc
	other   domain.SessionDoc
}

func rootFixtureMessage(role domain.Role, text string) domain.CIREvent {
	return domain.CIREvent{Kind: domain.EventMessage, Role: role, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}
}

func rootFixtureObjectPath(dir string, repo domain.ContentHash, kind string, hash domain.ContentHash) string {
	return filepath.Join(dir, "repos", strings.TrimPrefix(string(repo), "sha256:"), "objects", kind, strings.TrimPrefix(string(hash), "sha256:"))
}

func rootFixtureSeedDoc(test *testing.T, fixture *rootFixtureState, events ...domain.CIREvent) (domain.SessionDoc, domain.ConversationManifest) {
	test.Helper()
	document := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "document-query-root-fixture"},
		Events:   append([]domain.CIREvent{}, events...),
	}
	for position := range document.Events {
		document.Events[position].Seq = position
	}
	manifest, bodies, err := domain.ConversationManifestForCIR(document)
	if err != nil {
		test.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := fixture.storage.PutChunks(fixture.ctx, fixture.repo, bodies); err != nil {
		test.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		test.Fatal(err)
	}
	path := rootFixtureObjectPath(fixture.dir, fixture.repo, "docs", hash)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.PutSnapshot(fixture.ctx, domain.Snapshot{
		ID: hash, RepoID: fixture.repo, DocHash: hash, DocIdentity: domain.DocumentIdentityRootV1, Branch: "main",
	}); err != nil {
		test.Fatal(err)
	}
	return domain.SessionDoc{Hash: hash, Identity: domain.DocumentIdentityRootV1, CIR: document}, manifest
}

func rootFixtureNew(test *testing.T) *rootFixtureState {
	test.Helper()
	fixture := &rootFixtureState{
		ctx: inbound.WithSystemActor(context.Background()), repo: domain.HashContent([]byte("document-query-root-fixture-repository")), dir: test.TempDir(),
	}
	fixture.storage = store.NewFSStore(fixture.dir)
	fixture.target, _ = rootFixtureSeedDoc(test, fixture,
		rootFixtureMessage(domain.RoleUser, "requested needle \ud55c\uae00"), rootFixtureMessage(domain.RoleAssistant, "answer"))
	fixture.base, _ = rootFixtureSeedDoc(test, fixture, fixture.target.CIR.Events[0])
	fixture.other, _ = rootFixtureSeedDoc(test, fixture, rootFixtureMessage(domain.RoleUser, "other root"))
	fixture.reader = &rootFixtureReader{
		FSStore: fixture.storage, snapshotFailures: make(map[domain.ContentHash]error),
		projectionReplacements: make(map[domain.ContentHash]domain.ContentHash), projectionFailures: make(map[domain.ContentHash]error),
	}
	queries, err := documentquery.New(documentquery.Config{
		Profile: documentquery.VerifiedRoots, Scope: rootFixtureScope{}, Snapshots: fixture.reader,
		Legacy: fixture.reader, Index: fixture.reader, Verified: fixture.reader, RootEvents: fixture.reader, AtomicRead: true,
	})
	if err != nil {
		test.Fatal(err)
	}
	fixture.queries = queries
	return fixture
}

func rootFixtureRequireNoFallback(test *testing.T, fixture *rootFixtureState) {
	test.Helper()
	if len(fixture.reader.fallbackCalls) != 0 {
		test.Fatal("root validation fell back to legacy/index reads", fixture.reader.fallbackCalls)
	}
}

func rootFixtureRequireEmptyPage(test *testing.T, page domain.DocEventPage, err, want error) {
	test.Helper()
	if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.DocEventPage{}) {
		test.Fatal("root error exposed a partial page or changed its cause", page, err, want)
	}
}

func TestRootEventsQueriesAcceptCurrentMatchingProjection(test *testing.T) {
	fixture := rootFixtureNew(test)
	page, err := fixture.queries.ReadDocEvents(fixture.ctx, fixture.repo, fixture.target.Hash, fixture.base.Hash, -1, 1)
	if err != nil || page.Hash != fixture.target.Hash || page.Inherited != 1 || page.Total != 2 || page.Offset != 1 || page.Next != -1 ||
		len(page.Events) != 1 || !reflect.DeepEqual(page.Events[0], fixture.target.CIR.Events[1]) {
		test.Fatal("independent Queries lost the real root projection or inherited prefix", page, err)
	}
	page, err = fixture.queries.ReadDocEvents(fixture.ctx, fixture.repo, fixture.target.Hash, fixture.target.Hash, -1, 1)
	if err != nil || page.Hash != fixture.target.Hash || page.Inherited != 2 || page.Total != 2 || page.Next != -1 || len(page.Events) != 0 {
		test.Fatal("matching self-base projection did not exhaust the inherited root", page, err)
	}
	rootFixtureRequireNoFallback(test, fixture)
	if len(fixture.reader.verifiedCalls) != 0 {
		test.Fatal("root event projection materialized the complete document")
	}
}

func TestRootEventsQueriesRejectZeroAndMismatchedProjectionReferences(test *testing.T) {
	for _, mode := range []string{"zero", "mismatched"} {
		for _, selected := range []string{"target", "self-base", "target-after-base", "base"} {
			test.Run(mode+"/"+selected, func(test *testing.T) {
				fixture := rootFixtureNew(test)
				var base domain.ContentHash
				failedHash := fixture.target.Hash
				switch selected {
				case "self-base":
					base = fixture.target.Hash
				case "target-after-base", "base":
					base = fixture.base.Hash
				}
				if selected == "base" {
					failedHash = fixture.base.Hash
				}
				page, err := fixture.queries.ReadDocEvents(fixture.ctx, fixture.repo, fixture.target.Hash, base, 0, 1)
				if err != nil || len(page.Events) != 1 || !reflect.DeepEqual(page.Events[0], fixture.target.CIR.Events[0]) {
					test.Fatal("valid stored projection fixture was not readable", page, err)
				}
				replacement := domain.ContentHash("")
				if mode == "mismatched" {
					replacement = fixture.other.Hash
				}
				fixture.reader.projectionReplacements[failedHash] = replacement
				before := len(fixture.reader.projectionCalls)
				page, err = fixture.queries.ReadDocEvents(fixture.ctx, fixture.repo, fixture.target.Hash, base, 0, 1)
				rootFixtureRequireEmptyPage(test, page, err, domain.ErrIntegrity)
				rootFixtureRequireNoFallback(test, fixture)
				calls := fixture.reader.projectionCalls[before:]
				if len(calls) == 0 || calls[len(calls)-1].hash != failedHash || len(fixture.reader.verifiedCalls) != 0 {
					test.Fatal("invalid root reference was bypassed or retried through a full proof", calls)
				}
				if base == failedHash && (!calls[0].hashesOnly || len(calls) != 1) {
					test.Fatal("invalid base reference reached target paging", calls)
				}
			})
		}
	}
}

type rootFixtureReadCase struct {
	name  string
	empty any
	read  func(*rootFixtureState) (any, error)
}

func rootFixtureReadCases() []rootFixtureReadCase {
	return []rootFixtureReadCase{
		{name: "events", empty: domain.DocEventPage{}, read: func(fixture *rootFixtureState) (any, error) {
			return fixture.queries.ReadDocEvents(fixture.ctx, fixture.repo, fixture.target.Hash, "", 0, 1)
		}},
		{name: "fragments", empty: domain.DocFragmentPage{}, read: func(fixture *rootFixtureState) (any, error) {
			return fixture.queries.ReadDocFragments(fixture.ctx, fixture.repo, fixture.target.Hash, 0, 0, 1, 64<<10)
		}},
		{name: "search", empty: []domain.DocEventIndex(nil), read: func(fixture *rootFixtureState) (any, error) {
			return fixture.queries.SearchDocEvents(fixture.ctx, fixture.repo, fixture.target.Hash, "needle", -1, 10)
		}},
	}
}

func TestRootEventsQueriesRejectCurrentSnapshotErrorsAtomically(test *testing.T) {
	for _, operation := range rootFixtureReadCases() {
		for _, failure := range []error{errors.New("current root snapshot failed"), domain.ErrIntegrity, domain.ErrUnauthorized, context.Canceled} {
			test.Run(operation.name+"/"+failure.Error(), func(test *testing.T) {
				fixture := rootFixtureNew(test)
				warm, err := operation.read(fixture)
				if err != nil || reflect.DeepEqual(warm, operation.empty) {
					test.Fatal("real root query failed before the current snapshot error", warm, err)
				}
				beforeSnapshots := len(fixture.reader.snapshotCalls)
				beforeProjections, beforeVerified := len(fixture.reader.projectionCalls), len(fixture.reader.verifiedCalls)
				fixture.reader.snapshotFailures[fixture.target.Hash] = failure
				result, err := operation.read(fixture)
				if !errors.Is(err, failure) || !reflect.DeepEqual(result, operation.empty) {
					test.Fatal("current snapshot error exposed warmed or partial root data", result, err, failure)
				}
				if len(fixture.reader.snapshotCalls) <= beforeSnapshots || len(fixture.reader.projectionCalls) != beforeProjections || len(fixture.reader.verifiedCalls) != beforeVerified {
					test.Fatal("current snapshot failure was skipped or a content read followed it")
				}
				rootFixtureRequireNoFallback(test, fixture)
				delete(fixture.reader.snapshotFailures, fixture.target.Hash)
				recovered, err := operation.read(fixture)
				if err != nil || !reflect.DeepEqual(recovered, warm) {
					test.Fatal("transient snapshot failure changed the original stored root", recovered, err)
				}
			})
		}
	}
}

func TestRootEventsQueriesRejectBaseSnapshotAndProjectionErrorsAtomically(test *testing.T) {
	failure := errors.New("current base root read failed")
	for _, phase := range []string{"snapshot", "projection"} {
		test.Run(phase, func(test *testing.T) {
			fixture := rootFixtureNew(test)
			if phase == "snapshot" {
				fixture.reader.snapshotFailures[fixture.base.Hash] = failure
			} else {
				fixture.reader.projectionFailures[fixture.base.Hash] = failure
			}
			page, err := fixture.queries.ReadDocEvents(fixture.ctx, fixture.repo, fixture.target.Hash, fixture.base.Hash, -1, 1)
			rootFixtureRequireEmptyPage(test, page, err, failure)
			rootFixtureRequireNoFallback(test, fixture)
			for _, call := range fixture.reader.projectionCalls {
				if call.hash != fixture.base.Hash || !call.hashesOnly {
					test.Fatal("failed current base read reached target projection", call)
				}
			}
			if len(fixture.reader.verifiedCalls) != 0 || phase == "projection" && len(fixture.reader.projectionCalls) != 1 {
				test.Fatal("base error was replaced with another proof path")
			}
		})
	}
}

func TestRootEventsQueriesRereadCurrentUnrequestedTailWithoutFallback(test *testing.T) {
	fixture := rootFixtureNew(test)
	document, manifest := rootFixtureSeedDoc(test, fixture,
		rootFixtureMessage(domain.RoleUser, "first needle"), rootFixtureMessage(domain.RoleAssistant, strings.Repeat("tail", 200000)))
	if len(manifest.Chunks) < 2 {
		test.Fatal("current-tail fixture did not span multiple authenticated chunks")
	}
	page, err := fixture.queries.ReadDocEvents(fixture.ctx, fixture.repo, document.Hash, "", 0, 1)
	if err != nil || len(page.Events) != 1 || !reflect.DeepEqual(page.Events[0], document.CIR.Events[0]) {
		test.Fatal("real root page failed before stored tail corruption", page, err)
	}
	tail := manifest.Chunks[len(manifest.Chunks)-1]
	path := rootFixtureObjectPath(fixture.dir, fixture.repo, "chunks", tail.Hash)
	if err := os.WriteFile(path, []byte("corrupt current root tail"), 0600); err != nil {
		test.Fatal(err)
	}
	before := len(fixture.reader.projectionCalls)
	page, err = fixture.queries.ReadDocEvents(fixture.ctx, fixture.repo, document.Hash, "", 0, 1)
	rootFixtureRequireEmptyPage(test, page, err, domain.ErrIntegrity)
	rootFixtureRequireNoFallback(test, fixture)
	if len(fixture.reader.projectionCalls) != before+1 || len(fixture.reader.verifiedCalls) != 0 {
		test.Fatal("warmed root projection skipped current bytes or used a fallback proof")
	}
}
