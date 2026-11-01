package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type documentQueryScopeStore struct {
	*graphReadTransactions
	repositories atomic.Int64
	snapshots    atomic.Int64
	payloads     atomic.Int64
	required     domain.DocumentIdentity
	payloadError error
	finishError  error
	observe      func(context.Context) error
}

func (storage *documentQueryScopeStore) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
	gate, _ := ctx.Value(resourceAdmissionKey{}).(*resourceAdmission)
	if permit == nil || permit.owner != gate || !permit.live.Load() || permit.class != ResourceBulk {
		return errors.New("document query read escaped its live shared bulk permit")
	}
	if !storage.InTransaction(ctx) || storage.active.Load() != 1 {
		return errors.New("document query read escaped its pinned repository snapshot")
	}
	if storage.observe != nil {
		return storage.observe(ctx)
	}
	return nil
}

func (storage *documentQueryScopeStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	nested := storage.InTransaction(ctx)
	err := storage.graphReadTransactions.WithinReadSnapshot(ctx, read)
	if err == nil && !nested {
		return storage.finishError
	}
	return err
}

func (storage *documentQueryScopeStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	storage.repositories.Add(1)
	if err := storage.check(ctx); err != nil {
		return domain.Repo{}, err
	}
	metadata, err := storage.FSStore.GetRepo(ctx, repo)
	metadata.RequiredDocIdentity = storage.required
	return metadata, err
}

func (storage *documentQueryScopeStore) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	storage.snapshots.Add(1)
	if err := storage.check(ctx); err != nil {
		return domain.Snapshot{}, err
	}
	return storage.FSStore.GetSnapshot(ctx, repo, hash)
}

func (storage *documentQueryScopeStore) checkPayload(ctx context.Context) error {
	storage.payloads.Add(1)
	if err := storage.check(ctx); err != nil {
		return err
	}
	return storage.payloadError
}

func (storage *documentQueryScopeStore) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := storage.checkPayload(ctx); err != nil {
		return domain.SessionDoc{}, err
	}
	return storage.FSStore.GetDoc(ctx, repo, hash)
}

func (storage *documentQueryScopeStore) GetDocManifest(ctx context.Context, repo, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	if err := storage.checkPayload(ctx); err != nil {
		return domain.DocChunkManifest{}, err
	}
	return storage.FSStore.GetDocManifest(ctx, repo, hash)
}

func (storage *documentQueryScopeStore) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	if err := storage.checkPayload(ctx); err != nil {
		return nil, err
	}
	return storage.FSStore.GetChunk(ctx, repo, hash)
}

func (storage *documentQueryScopeStore) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	if err := storage.checkPayload(ctx); err != nil {
		return domain.DocReadIndex{}, err
	}
	return storage.FSStore.DocReadIndex(ctx, repo, hash)
}

func (storage *documentQueryScopeStore) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, query string, after, limit int) ([]domain.DocEventIndex, error) {
	if err := storage.checkPayload(ctx); err != nil {
		return nil, err
	}
	return storage.FSStore.SearchDocEvents(ctx, repo, hash, query, after, limit)
}

type documentQueryScopeFailedBlobs struct {
	outbound.BlobStore
	calls atomic.Int64
	err   error
}

func (storage *documentQueryScopeFailedBlobs) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	storage.calls.Add(1)
	return domain.SessionDoc{}, storage.err
}

func (storage *documentQueryScopeFailedBlobs) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	storage.calls.Add(1)
	return domain.DocChunkManifest{}, storage.err
}

func (storage *documentQueryScopeFailedBlobs) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	storage.calls.Add(1)
	return nil, storage.err
}

func (storage *documentQueryScopeFailedBlobs) DocReadIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocReadIndex, error) {
	storage.calls.Add(1)
	return domain.DocReadIndex{}, storage.err
}

func (storage *documentQueryScopeFailedBlobs) SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error) {
	storage.calls.Add(1)
	return nil, storage.err
}

type documentQueryScopeFailedMetadata struct {
	outbound.MetadataStore
	calls atomic.Int64
	err   error
}

func (storage *documentQueryScopeFailedMetadata) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	storage.calls.Add(1)
	return domain.Repo{}, storage.err
}

func (storage *documentQueryScopeFailedMetadata) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	storage.calls.Add(1)
	return domain.Snapshot{}, storage.err
}

type documentQueryScopeFixture struct {
	service  *Service
	storage  *documentQueryScopeStore
	repo     domain.ContentHash
	document domain.SessionDoc
	ctx      context.Context
	gate     *resourceAdmission
}

func newDocumentQueryScopeFixture(test *testing.T) documentQueryScopeFixture {
	test.Helper()
	filesystem := store.NewFSStore(test.TempDir())
	repo := hh(test.Name())
	if _, err := filesystem.PutRepo(systemTestContext(), domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	document := seedLegacyFindDocument(test, filesystem, repo, []domain.CIREvent{
		historyMessage(domain.RoleUser, "first needle \ud55c🙂"),
		historyMessage(domain.RoleAssistant, "second needle"),
	})
	if err := filesystem.PutSnapshot(systemTestContext(), domain.Snapshot{ID: document.Hash, RepoID: repo,
		DocHash: document.Hash, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	storage := &documentQueryScopeStore{graphReadTransactions: &graphReadTransactions{FSStore: filesystem}}
	_, gate := admissionTestContext(time.Second)
	ctx := context.WithValue(systemTestContext(), resourceAdmissionKey{}, gate)
	return documentQueryScopeFixture{service: NewService(storage, storage, nil, nil, nil), storage: storage,
		repo: repo, document: document, ctx: ctx, gate: gate}
}

func documentQueryScopeQueries(test *testing.T, fixture documentQueryScopeFixture, constructor bool) inbound.DocumentEventQueries {
	test.Helper()
	var queries inbound.DocumentEventQueries
	var err error
	if constructor {
		scope := &documentQueryScope{metadata: fixture.storage, transactions: fixture.storage, presence: fixture.storage,
			supported: fixture.service.DocumentIdentitiesSupported()}
		queries, err = documentquery.New(documentquery.Config{Profile: documentquery.LegacyCompatibility, Scope: scope,
			Snapshots: struct{ documentquery.SnapshotReader }{fixture.storage},
			Legacy:    struct{ documentquery.LegacyReader }{fixture.storage},
			Index:     fixture.storage, AtomicRead: true})
	} else {
		queries, err = fixture.service.DocumentEventQueries()
	}
	if err != nil || queries == nil {
		test.Fatal("construct independent document queries", err)
	}
	if _, independent := queries.(*documentquery.Queries); !independent {
		test.Fatalf("document accessor retained a service facade instead of the new query owner: %T", queries)
	}
	return queries
}

type documentQueryScopeOperation struct {
	name string
	read func(context.Context) (any, error)
	zero any
}

func documentQueryScopeOperations(queries inbound.DocumentEventQueries, fixture documentQueryScopeFixture) []documentQueryScopeOperation {
	return []documentQueryScopeOperation{
		{name: "events", zero: domain.DocEventPage{}, read: func(ctx context.Context) (any, error) {
			return queries.ReadDocEvents(ctx, fixture.repo, fixture.document.Hash, "", 0, 2)
		}},
		{name: "fragments", zero: domain.DocFragmentPage{}, read: func(ctx context.Context) (any, error) {
			return queries.ReadDocFragments(ctx, fixture.repo, fixture.document.Hash, 0, 0, 2, 4096)
		}},
		{name: "search", zero: []domain.DocEventIndex(nil), read: func(ctx context.Context) (any, error) {
			return queries.SearchDocEvents(ctx, fixture.repo, fixture.document.Hash, "needle", -1, 2)
		}},
	}
}

func documentQueryScopeAssertSuccess(test *testing.T, operation documentQueryScopeOperation, ctx context.Context, document domain.SessionDoc) {
	test.Helper()
	result, err := operation.read(ctx)
	if err != nil {
		test.Fatal(operation.name, err)
	}
	switch page := result.(type) {
	case domain.DocEventPage:
		if page.Hash != document.Hash || page.Total != 2 || page.Next != -1 || !reflect.DeepEqual(page.Events, document.CIR.Events) {
			test.Fatal("independent query changed the original event page", page)
		}
	case domain.DocFragmentPage:
		if page.Total != 2 || len(page.Fragments) != 2 || page.NextIndex != 2 || page.NextOffset != 0 {
			test.Fatal("independent query changed fragment boundaries", page)
		}
		for position, fragment := range page.Fragments {
			var event domain.CIREvent
			if err := json.Unmarshal([]byte(fragment.JSON), &event); err != nil || fragment.Index != position || fragment.Offset != 0 ||
				!fragment.Complete || !reflect.DeepEqual(event, document.CIR.Events[position]) {
				test.Fatal("independent query did not read the original exact event fragment", fragment, err)
			}
		}
	case []domain.DocEventIndex:
		if len(page) != 2 || page[0].Index != 0 || page[1].Index != 1 || page[0].Role != string(document.CIR.Events[0].Role) || page[1].Role != string(document.CIR.Events[1].Role) {
			test.Fatal("independent query lost original indexed search results", page)
		}
	default:
		test.Fatalf("unexpected document query result: %T", result)
	}
}

func TestDocumentEventQueriesOwnOriginalReadPortsWithoutServiceBackReference(test *testing.T) {
	for _, constructor := range []bool{false, true} {
		name := "accessor"
		if constructor {
			name = "constructor"
		}
		test.Run(name, func(test *testing.T) {
			fixture := newDocumentQueryScopeFixture(test)
			queries := documentQueryScopeQueries(test, fixture, constructor)
			failure := errors.New("replacement service ports must not be dereferenced")
			blobs := &documentQueryScopeFailedBlobs{err: failure}
			metadata := &documentQueryScopeFailedMetadata{err: failure}
			fixture.service.blobs, fixture.service.meta = blobs, metadata
			for _, operation := range documentQueryScopeOperations(queries, fixture) {
				before := fixture.storage.reads.Load()
				payloads := fixture.storage.payloads.Load()
				documentQueryScopeAssertSuccess(test, operation, fixture.ctx, fixture.document)
				if fixture.storage.reads.Load() != before+1 || fixture.storage.active.Load() != 0 || fixture.storage.payloads.Load() <= payloads {
					test.Fatal("query did not read its original ports in one owned snapshot", operation.name)
				}
				assertResourcesReleased(test, fixture.gate)
			}
			if blobs.calls.Load() != 0 || metadata.calls.Load() != 0 || fixture.storage.repositories.Load() == 0 || fixture.storage.snapshots.Load() == 0 {
				test.Fatal("extracted queries followed service replacement ports or bypassed original metadata", blobs.calls.Load(), metadata.calls.Load())
			}
		})
	}
}

func TestDocumentEventQueriesQueueOnExistingBulkGateBeforePinnedReads(test *testing.T) {
	for position, name := range []string{"events", "fragments", "search"} {
		test.Run(name, func(test *testing.T) {
			fixture := newDocumentQueryScopeFixture(test)
			fixture.gate.wait = 5 * time.Second
			queries := documentQueryScopeQueries(test, fixture, false)
			operation := documentQueryScopeOperations(queries, fixture)[position]
			_, release, err := AcquireResources(fixture.ctx, ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer release()
			queued, cancel := context.WithCancel(fixture.ctx)
			defer cancel()
			type outcome struct {
				result any
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := operation.read(queued)
				done <- outcome{result: result, err: err}
			}()
			waitResourceQueue(test, fixture.gate, ResourceBulk, 1)
			if fixture.storage.reads.Load() != 0 || fixture.storage.repositories.Load() != 0 || fixture.storage.payloads.Load() != 0 {
				test.Fatal("queued document query opened a snapshot or read a port before admission")
			}
			_, releaseInteractive, err := AcquireResources(fixture.ctx, ResourceInteractive)
			if err != nil {
				test.Fatal("queued query consumed the existing interactive reserve", err)
			}
			releaseInteractive()
			cancel()
			result := <-done
			if !errors.Is(result.err, context.Canceled) || !reflect.DeepEqual(result.result, operation.zero) || fixture.storage.reads.Load() != 0 {
				test.Fatal("canceled queued query returned payload or opened a read", result)
			}
			release()
			assertResourcesReleased(test, fixture.gate)
			documentQueryScopeAssertSuccess(test, operation, fixture.ctx, fixture.document)
			assertResourcesReleased(test, fixture.gate)
		})
	}
}

func TestDocumentEventQueriesReuseLivePermitButNotReleasedContext(test *testing.T) {
	fixture := newDocumentQueryScopeFixture(test)
	fixture.gate.wait = 5 * time.Millisecond
	queries := documentQueryScopeQueries(test, fixture, false)
	bound, release, err := AcquireResources(fixture.ctx, ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer release()
	permit := bound.Value(resourcePermitKey{}).(*resourcePermit)
	fixture.storage.observe = func(read context.Context) error {
		if read.Value(resourcePermitKey{}) != permit {
			return errors.New("nested document query replaced its existing live permit")
		}
		return nil
	}
	for _, operation := range documentQueryScopeOperations(queries, fixture) {
		documentQueryScopeAssertSuccess(test, operation, bound, fixture.document)
		fixture.gate.mu.Lock()
		active, bulk := fixture.gate.active, fixture.gate.bulk
		fixture.gate.mu.Unlock()
		if !permit.live.Load() || active != 1 || bulk != 1 {
			test.Fatal("nested query released or duplicated its caller's permit", operation.name, active, bulk)
		}
	}
	release()
	assertResourcesReleased(test, fixture.gate)
	_, releaseOther, err := AcquireResources(fixture.ctx, ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer releaseOther()
	reads := fixture.storage.reads.Load()
	for _, operation := range documentQueryScopeOperations(queries, fixture) {
		result, err := operation.read(bound)
		if !errors.Is(err, domain.ErrResourceOverloaded) || !reflect.DeepEqual(result, operation.zero) || fixture.storage.reads.Load() != reads {
			test.Fatal("released document context bypassed the currently occupied bulk slot", operation.name, result, err)
		}
	}
	releaseOther()
	assertResourcesReleased(test, fixture.gate)
}

type documentQueryScopeAuthorizationKey struct{}

func TestDocumentEventQueriesReauthorizeCurrentCallbackInsideSameScope(test *testing.T) {
	for position, name := range []string{"events", "fragments", "search"} {
		test.Run(name, func(test *testing.T) {
			fixture := newDocumentQueryScopeFixture(test)
			queries := documentQueryScopeQueries(test, fixture, false)
			operation := documentQueryScopeOperations(queries, fixture)[position]
			allowed, calls, admissions := true, 0, 0
			marker := &struct{}{}
			ctx := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
				admissions++
				if repo != fixture.repo || fixture.storage.InTransaction(bound) || fixture.storage.active.Load() != 0 {
					return bound, errors.New("normal authentication entered the pinned read transaction")
				}
				permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
				if permit == nil || permit.owner != fixture.gate || !permit.live.Load() {
					return bound, errors.New("authorization ran without the same admitted permit")
				}
				return inbound.WithDocumentSnapshotReadAuthorizer(bound, func(pinned context.Context, requested domain.ContentHash) (context.Context, error) {
					calls++
					if requested != fixture.repo || !fixture.storage.InTransaction(pinned) || fixture.storage.active.Load() != 1 || pinned.Value(resourcePermitKey{}) != permit {
						return pinned, errors.New("authorization escaped the requested pinned document scope")
					}
					if !allowed {
						return pinned, domain.ErrForbidden
					}
					return context.WithValue(pinned, documentQueryScopeAuthorizationKey{}, marker), nil
				}), nil
			})
			fixture.storage.observe = func(bound context.Context) error {
				if bound.Value(documentQueryScopeAuthorizationKey{}) != marker {
					return errors.New("document port lost the current authorization callback context")
				}
				return nil
			}
			documentQueryScopeAssertSuccess(test, operation, ctx, fixture.document)
			if admissions != 1 || calls < admissions {
				test.Fatal("query skipped the current admission or snapshot authorization callback", calls, admissions)
			}
			allowed = false
			before, payloads := calls, fixture.storage.payloads.Load()
			result, err := operation.read(ctx)
			if !errors.Is(err, domain.ErrForbidden) || calls <= before || admissions != 2 || calls < admissions || !reflect.DeepEqual(result, operation.zero) || fixture.storage.payloads.Load() != payloads {
				test.Fatal("warm independent query reused revoked authority or exposed payload", result, err, calls)
			}
			if fixture.storage.active.Load() != 0 {
				test.Fatal("authorization failure retained a pinned read")
			}
			assertResourcesReleased(test, fixture.gate)
		})
	}
}

func TestDocumentEventQueriesRequireCurrentSnapshotAuthorization(test *testing.T) {
	for _, mode := range []string{"missing", "normal only", "normal rejected", "snapshot revoked", "snapshot allowed"} {
		test.Run(mode, func(test *testing.T) {
			fixture := newDocumentQueryScopeFixture(test)
			scope := &documentQueryScope{metadata: fixture.storage, transactions: fixture.storage, presence: fixture.storage,
				supported: fixture.service.DocumentIdentitiesSupported(), requireAuthorization: true}
			queries, err := documentquery.New(documentquery.Config{Profile: documentquery.LegacyCompatibility,
				Scope: scope, Snapshots: fixture.storage, Legacy: fixture.storage, Index: fixture.storage, AtomicRead: true})
			if err != nil {
				test.Fatal(err)
			}
			ctx := inbound.WithRepositoryActor(fixture.ctx, "member")
			expected := error(domain.ErrUnauthorized)
			admissions, snapshots := 0, 0
			if mode != "missing" {
				ctx = inbound.WithDocumentReadAuthorizer(ctx, func(admitted context.Context, repo domain.ContentHash) (context.Context, error) {
					admissions++
					if fixture.storage.InTransaction(admitted) || repo != fixture.repo {
						test.Fatal("normal authorization used a pinned transaction or wrong repository")
					}
					if mode == "normal rejected" {
						return admitted, domain.ErrForbidden
					}
					if mode == "normal only" {
						return admitted, nil
					}
					return inbound.WithDocumentSnapshotReadAuthorizer(admitted, func(pinned context.Context, requested domain.ContentHash) (context.Context, error) {
						snapshots++
						if !fixture.storage.InTransaction(pinned) || requested != fixture.repo {
							test.Fatal("current authorization escaped its pinned repository snapshot")
						}
						if mode == "snapshot revoked" {
							return pinned, domain.ErrForbidden
						}
						return pinned, nil
					}), nil
				})
				switch mode {
				case "normal only":
					expected = domain.ErrCatalogUnsupported
				case "normal rejected", "snapshot revoked":
					expected = domain.ErrForbidden
				case "snapshot allowed":
					expected = nil
				}
			}
			for _, operation := range documentQueryScopeOperations(queries, fixture) {
				if expected == nil {
					documentQueryScopeAssertSuccess(test, operation, ctx, fixture.document)
				} else if result, err := operation.read(ctx); !errors.Is(err, expected) || !reflect.DeepEqual(result, operation.zero) {
					test.Fatal("strict document read accepted missing or revoked snapshot authority", operation.name, result, err)
				}
				if fixture.storage.active.Load() != 0 {
					test.Fatal("authorization retained a pinned transaction")
				}
				assertResourcesReleased(test, fixture.gate)
			}
			if expected != nil && fixture.storage.payloads.Load() != 0 {
				test.Fatal("rejected authorization read document payload")
			}
			if mode == "missing" && (admissions != 0 || snapshots != 0 || fixture.storage.reads.Load() != 0) {
				test.Fatal("missing authorization performed repository work")
			}
			if mode == "normal rejected" && fixture.storage.reads.Load() != 0 {
				test.Fatal("normal rejection opened a read snapshot")
			}
			if (mode == "snapshot revoked" || mode == "snapshot allowed") && (admissions != 3 || snapshots < admissions) {
				test.Fatal("document operations omitted normal or snapshot authorization", admissions, snapshots)
			}
		})
	}
}

func TestDocumentQueriesSystemActorDoesNotMoveGeneralAuthenticationIntoSnapshot(test *testing.T) {
	fixture := newDocumentQueryScopeFixture(test)
	queries := documentQueryScopeQueries(test, fixture, false)
	calls := 0
	ctx := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		calls++
		if fixture.storage.InTransaction(bound) {
			test.Fatal("general authentication ran inside a system actor's pinned snapshot")
		}
		return bound, nil
	})
	for position, operation := range documentQueryScopeOperations(queries, fixture) {
		result, err := operation.read(ctx)
		if !errors.Is(err, domain.ErrCatalogUnsupported) || !reflect.DeepEqual(result, operation.zero) || calls != position+1 || fixture.storage.payloads.Load() != 0 {
			test.Fatal("system actor reused write-capable authentication as snapshot authority", operation.name, result, err, calls)
		}
		assertResourcesReleased(test, fixture.gate)
	}
}

func TestDocumentEventQueriesVerifiedProfileRequiresPinnedTransactions(test *testing.T) {
	filesystem := store.NewFSStore(test.TempDir())
	service := NewService(filesystem, filesystem, nil, nil, nil)
	if queries, err := service.DocumentEventQueriesForProfile(documentquery.VerifiedRoots); queries != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("verified profile accepted a store without pinned repository transactions", queries, err)
	}
	if queries, err := service.DocumentEventQueriesForProfile(documentquery.LegacyCompatibility); queries == nil || err != nil {
		test.Fatal("explicit native compatibility was disabled", queries, err)
	}
}

func TestDocumentEventQueriesRecheckPinnedIdentityBeforeWarmPayload(test *testing.T) {
	for position, name := range []string{"events", "fragments", "search"} {
		test.Run(name, func(test *testing.T) {
			fixture := newDocumentQueryScopeFixture(test)
			queries := documentQueryScopeQueries(test, fixture, false)
			operation := documentQueryScopeOperations(queries, fixture)[position]
			documentQueryScopeAssertSuccess(test, operation, fixture.ctx, fixture.document)
			payloads, snapshots, checks := fixture.storage.payloads.Load(), fixture.storage.snapshots.Load(), fixture.storage.repositories.Load()
			fixture.storage.required = domain.DocumentIdentityRootV1
			result, err := operation.read(fixture.ctx)
			if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || !reflect.DeepEqual(result, operation.zero) ||
				fixture.storage.payloads.Load() != payloads || fixture.storage.snapshots.Load() != snapshots || fixture.storage.repositories.Load() <= checks {
				test.Fatal("independent query froze an old identity requirement or read before the current check", result, err)
			}
			if fixture.storage.active.Load() != 0 {
				test.Fatal("identity failure retained a pinned read")
			}
			assertResourcesReleased(test, fixture.gate)
		})
	}
}

func TestDocumentEventQueriesFailureAndCancellationReleasePermitAndAtomicResult(test *testing.T) {
	for position, name := range []string{"events", "fragments", "search"} {
		for _, failure := range []string{"payload", "read snapshot", "canceled", "authorization cancellation"} {
			test.Run(name+"/"+failure, func(test *testing.T) {
				fixture := newDocumentQueryScopeFixture(test)
				queries := documentQueryScopeQueries(test, fixture, false)
				operation := documentQueryScopeOperations(queries, fixture)[position]
				ctx := fixture.ctx
				want := errors.New("original document read failed")
				switch failure {
				case "payload":
					fixture.storage.payloadError = want
				case "read snapshot":
					fixture.storage.finishError = want
				case "canceled":
					canceled, cancel := context.WithCancel(ctx)
					cancel()
					ctx, want = canceled, context.Canceled
				case "authorization cancellation":
					canceled, cancel := context.WithCancel(ctx)
					defer cancel()
					ctx = inbound.WithDocumentReadAuthorizer(canceled, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
						cancel()
						return bound, bound.Err()
					})
					want = context.Canceled
				}
				result, err := operation.read(ctx)
				if !errors.Is(err, want) || !reflect.DeepEqual(result, operation.zero) || fixture.storage.active.Load() != 0 {
					test.Fatal("failed document scope changed its error, exposed a partial result or retained a snapshot", result, err)
				}
				if (failure == "payload" || failure == "read snapshot") && fixture.storage.payloads.Load() == 0 {
					test.Fatal("failure fixture did not reach its original payload port")
				}
				if (failure == "canceled" || failure == "authorization cancellation") && fixture.storage.payloads.Load() != 0 {
					test.Fatal("canceled scope reached document payload ports")
				}
				if failure == "canceled" && fixture.storage.reads.Load() != 0 {
					test.Fatal("already canceled query opened a snapshot before admission")
				}
				assertResourcesReleased(test, fixture.gate)
			})
		}
	}
}
