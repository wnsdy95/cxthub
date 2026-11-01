package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/archivequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func readServicesFixture(test *testing.T) (ReadServicesConfig, *store.FSStore, domain.ContentHash, domain.SessionDoc) {
	test.Helper()
	storage := store.NewFSStore(test.TempDir())
	repo := domain.HashContent([]byte(test.Name()))
	if _, err := storage.PutRepo(systemTestContext(), domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	document := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{
		{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "assembly needle"}}},
	})
	if err := storage.PutSnapshot(systemTestContext(), domain.Snapshot{ID: document.Hash, RepoID: repo, DocHash: document.Hash,
		Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	return ReadServicesConfig{Metadata: storage, Blobs: storage, DocumentProfile: documentquery.LegacyCompatibility,
		ContextProfile: inbound.ContextQueryCompatibility, SupportedDocumentIdentities: ReadDocumentIdentities(storage, storage)}, storage, repo, document
}

func TestReadServicesBindsIndependentQueriesAndLifetimeCaches(test *testing.T) {
	config, _, repo, document := readServicesFixture(test)
	services, err := NewReadServices(config)
	if err != nil {
		test.Fatal(err)
	}
	documents, documentOwner := services.Documents.(*documentquery.Queries)
	conversation, conversationOwner := services.ConversationFind.(*documentquery.Queries)
	contextQueries, contextOwner := services.Context.(*repositoryquery.ContextQueries)
	live, liveOwner := services.MemoryLive.(*memoryquery.LiveQueries)
	_, preparedOwner := services.PreparedMemory.(*memoryquery.PreparedQueries)
	_, snapshotOwner := services.MemorySnapshots.(*memoryquery.SnapshotQueries)
	_, archiveOwner := services.Archive.(*archivequery.ArchiveQueries)
	_, changeOwner := services.GitChanges.(*evidencequery.ChangeQueries)
	_, scanOwner := services.GitScans.(*evidencequery.ScanQueries)
	_, codeOwner := services.CodeApplicability.(*evidencequery.CodeQueries)
	if !documentOwner || !conversationOwner || documents != conversation || !contextOwner || !liveOwner ||
		!preparedOwner || !snapshotOwner || !archiveOwner || !changeOwner || !scanOwner || !codeOwner {
		test.Fatal("read assembly did not bind standalone query owners")
	}
	segmentPointer := reflect.ValueOf(contextQueries).Elem().FieldByName("cache").Pointer()
	deliveryPointer := reflect.ValueOf(live).Elem().FieldByName("delivery").Pointer()
	if segmentPointer != reflect.ValueOf(&services.segmentCache).Pointer() || deliveryPointer != reflect.ValueOf(&services.deliveryCache).Pointer() {
		test.Fatal("query owners did not retain their assembly's actual cache instances")
	}
	contributions := reflect.ValueOf(contextQueries).Elem().FieldByName("contributions").Elem().Pointer()
	if contributions != reflect.ValueOf(live).Elem().FieldByName("evidence").Pointer() {
		test.Fatal("context and live memory did not share their bound evidence queries")
	}
	config.Metadata, config.Blobs = nil, nil
	config.SupportedDocumentIdentities[0] = "invalid-after-construction"
	for range 2 {
		page, err := services.Documents.ReadDocEvents(systemTestContext(), repo, document.Hash, "", 0, 1)
		if err != nil || len(page.Events) != 1 {
			test.Fatal("document query rediscovered composition inputs", page, err)
		}
		if _, err := services.Context.QueryContext(systemTestContext(), repo, domain.ContextSelection{}); err != nil {
			test.Fatal(err)
		}
		if reflect.ValueOf(contextQueries).Elem().FieldByName("cache").Pointer() != segmentPointer ||
			reflect.ValueOf(live).Elem().FieldByName("delivery").Pointer() != deliveryPointer {
			test.Fatal("a read replaced a lifetime cache")
		}
	}
}

func TestReadServicesSharesOneActiveMemoryOwnerAndProcessPermit(test *testing.T) {
	config, _, repo, _ := readServicesFixture(test)
	services, err := NewReadServices(config)
	if err != nil {
		test.Fatal(err)
	}
	other, err := NewReadServices(config)
	if err != nil || other.memoryOwner == services.memoryOwner || &other.segmentCache == &services.segmentCache || &other.deliveryCache == &services.deliveryCache {
		test.Fatal("independent assemblies shared mutable ownership", err)
	}
	checks := 0
	ctx := inbound.WithRepositoryActor(context.Background(), "reader")
	ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, selected domain.ContentHash) (context.Context, error) {
		checks++
		permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
		if selected != repo || permit == nil || permit.owner != processResources || !permit.live.Load() {
			return bound, domain.ErrIntegrity
		}
		return bound, nil
	})
	var captured context.Context
	var original *preparedMemoryScope
	err = services.PreparedMemory.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		captured = bound
		original, _ = bound.Value(preparedMemoryScopeKey{}).(*preparedMemoryScope)
		if original == nil || original.ownerKey != services.memoryOwner || !original.active.Load() {
			test.Fatal("prepared read lost its assembly memory owner")
		}
		for _, reader := range []inbound.MemoryReadScope{services.MemorySnapshots, services.MemoryLive} {
			if err := reader.WithMemoryReadResources(bound, repo, func(current context.Context) error {
				if current.Value(preparedMemoryScopeKey{}) != original || current.Value(resourcePermitKey{}) != bound.Value(resourcePermitKey{}) {
					test.Fatal("nested memory read allocated another authority scope or permit")
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || checks != 1 || original == nil || original.active.Load() || original.permit.live.Load() {
		test.Fatal("active memory identity or shared permit outlived its read", err, checks)
	}
	err = services.MemoryLive.WithMemoryReadResources(captured, repo, func(current context.Context) error {
		if current.Value(preparedMemoryScopeKey{}) == original {
			test.Fatal("completed memory scope was reused")
		}
		return nil
	})
	if err != nil || checks != 2 {
		test.Fatal("completed read did not reauthorize under fresh admission", err, checks)
	}
}

func TestReadServicesChangedMemoryAuthorityCannotReuseActiveProof(test *testing.T) {
	config, _, repo, _ := readServicesFixture(test)
	services, err := NewReadServices(config)
	if err != nil {
		test.Fatal(err)
	}
	checks := 0
	ctx := inbound.WithDocumentReadAuthorizer(systemTestContext(), func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		checks++
		return bound, nil
	})
	err = services.PreparedMemory.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		changed := inbound.WithDocumentReadAuthorizer(bound, func(current context.Context, _ domain.ContentHash) (context.Context, error) {
			checks++
			return current, domain.ErrForbidden
		})
		return services.MemoryLive.WithMemoryReadResources(changed, repo, func(context.Context) error {
			test.Fatal("replaced authority reused an active proof")
			return nil
		})
	})
	if !errors.Is(err, domain.ErrForbidden) || checks != 2 {
		test.Fatal("changed authority did not fail closed", err, checks)
	}
}

func TestReadServicesDocumentResponseRetainsTheOriginalProcessPermit(test *testing.T) {
	config, _, repo, document := readServicesFixture(test)
	services, err := NewReadServices(config)
	if err != nil {
		test.Fatal(err)
	}
	var permit *resourcePermit
	ctx := inbound.WithDocumentReadAuthorizer(systemTestContext(), func(bound context.Context, selected domain.ContentHash) (context.Context, error) {
		permit, _ = bound.Value(resourcePermitKey{}).(*resourcePermit)
		if selected != repo || permit == nil || permit.owner != processResources || !permit.live.Load() {
			return bound, domain.ErrIntegrity
		}
		return bound, nil
	})
	var retained []func()
	ctx = inbound.WithDocumentResponseRetainer(ctx, func(release func()) bool {
		retained = append(retained, release)
		return true
	})
	defer func() {
		for _, release := range retained {
			release()
		}
	}()
	page, err := services.Documents.ReadDocFindFragment(ctx, repo, document.Hash, domain.DocFindFragmentRequest{
		Legacy: true, Total: 1, Budget: 4096,
	})
	if err != nil || page.JSON == "" || len(retained) != 1 || permit == nil || !permit.live.Load() {
		test.Fatal("document response released or replaced its permit before delivery", page, err, len(retained))
	}
	retained[0]()
	if permit.live.Load() {
		test.Fatal("document response completion retained process admission")
	}
}

type readServicesSnapshotKey struct{}

type readServicesSnapshotStore struct {
	*store.FSStore
	snapshots     int
	metadataReads int
	finishFailure error
}

func (storage *readServicesSnapshotStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if storage.InTransaction(ctx) {
		return read(ctx)
	}
	storage.snapshots++
	return errors.Join(read(context.WithValue(ctx, readServicesSnapshotKey{}, storage)), storage.finishFailure)
}

func (storage *readServicesSnapshotStore) InTransaction(ctx context.Context) bool {
	return ctx.Value(readServicesSnapshotKey{}) == storage
}

func (storage *readServicesSnapshotStore) InReadOnlyTransaction(ctx context.Context) bool {
	return storage.InTransaction(ctx)
}

func (storage *readServicesSnapshotStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	storage.metadataReads++
	permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
	if !storage.InTransaction(ctx) || permit == nil || !permit.live.Load() || permit.owner != processResources {
		return domain.Repo{}, domain.ErrIntegrity
	}
	return storage.FSStore.GetRepo(ctx, repo)
}

func TestReadServicesPinnedReadsUseSnapshotAuthorityAndClearFailedResults(test *testing.T) {
	config, filesystem, repo, document := readServicesFixture(test)
	storage := &readServicesSnapshotStore{FSStore: filesystem}
	config.Metadata, config.ContextProfile = storage, inbound.ContextQueryPinnedSnapshots
	services, err := NewReadServices(config)
	if err != nil || storage.snapshots != 0 || storage.metadataReads != 0 {
		test.Fatal("construction performed read work", err, storage.snapshots, storage.metadataReads)
	}
	general, snapshots := 0, 0
	ctx := inbound.WithDocumentReadAuthorizer(systemTestContext(), func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		general++
		return bound, domain.ErrForbidden
	})
	ctx = inbound.WithDocumentSnapshotReadAuthorizer(ctx, func(bound context.Context, selected domain.ContentHash) (context.Context, error) {
		snapshots++
		if selected != repo || !storage.InTransaction(bound) {
			return bound, domain.ErrIntegrity
		}
		return bound, nil
	})
	view, err := services.Context.QueryContext(ctx, repo, domain.ContextSelection{})
	if err != nil || view.Version != 1 {
		test.Fatal("pinned context escaped snapshot authority", view, err)
	}
	page, err := services.Documents.ReadDocEvents(ctx, repo, document.Hash, "", 0, 1)
	if err != nil || len(page.Events) != 1 || snapshots < 2 || general != 0 {
		test.Fatal("pinned document used general or foreign authority", page, err, general, snapshots)
	}
	if _, err := services.Archive.List(ctx, inbound.ListSnapshotsInput{RepoID: repo}); err != nil {
		test.Fatal("archive did not use the assembly's pinned authority", err)
	}
	if _, err := services.GitChanges.List(ctx, repo, "", 1); err != nil {
		test.Fatal("Git changes did not use the assembly's pinned authority", err)
	}
	if _, err := services.GitScans.ListScans(ctx, repo, "", 1); err != nil {
		test.Fatal("Git scans did not use the assembly's pinned authority", err)
	}
	selection := domain.CodeSelection{CodeCommit: strings.Repeat("a", 40), SourceCommit: strings.Repeat("b", 40), Paths: []string{"source.go"}}
	if _, err := services.CodeApplicability.QueryCodeApplicability(ctx, repo, selection); err != nil || general != 0 {
		test.Fatal("code applicability did not use the assembly's pinned authority", err, general)
	}
	storage.finishFailure = domain.ErrConflict
	view, err = services.Context.QueryContext(ctx, repo, domain.ContextSelection{})
	if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(view, domain.ContextQueryView{}) {
		test.Fatal("failed snapshot leaked a context result", view, err)
	}
	page, err = services.Documents.ReadDocEvents(ctx, repo, document.Hash, "", 0, 1)
	if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(page, domain.DocEventPage{}) {
		test.Fatal("failed snapshot leaked a document result", page, err)
	}
	changes, err := services.GitChanges.List(ctx, repo, "", 1)
	if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(changes, domain.GitChangePage{}) {
		test.Fatal("failed snapshot leaked Git change results", changes, err)
	}
	scans, err := services.GitScans.ListScans(ctx, repo, "", 1)
	if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(scans, domain.GitScanPage{}) {
		test.Fatal("failed snapshot leaked Git scan results", scans, err)
	}
	code, err := services.CodeApplicability.QueryCodeApplicability(ctx, repo, selection)
	if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(code, domain.CodeApplicability{}) {
		test.Fatal("failed snapshot leaked code applicability results", code, err)
	}
}

func TestReadServicesBorrowedSnapshotNeverCallsGeneralAuthority(test *testing.T) {
	config, filesystem, repo, _ := readServicesFixture(test)
	storage := &readServicesSnapshotStore{FSStore: filesystem}
	config.Metadata, config.ContextProfile = storage, inbound.ContextQueryPinnedSnapshots
	services, err := NewReadServices(config)
	if err != nil {
		test.Fatal(err)
	}
	general := 0
	err = storage.WithinReadSnapshot(systemTestContext(), func(bound context.Context) error {
		request := inbound.WithDocumentReadAuthorizer(bound, func(current context.Context, _ domain.ContentHash) (context.Context, error) {
			general++
			return current, nil
		})
		_, err := services.Context.QueryContext(request, repo, domain.ContextSelection{})
		if !errors.Is(err, domain.ErrCatalogUnsupported) {
			test.Fatal("borrowed context accepted general-only authority", err)
		}
		return services.MemoryLive.WithMemoryReadResources(request, repo, func(context.Context) error {
			test.Fatal("borrowed memory accepted general-only authority")
			return nil
		})
	})
	if !errors.Is(err, domain.ErrCatalogUnsupported) || general != 0 {
		test.Fatal("general authority entered a borrowed snapshot", err, general)
	}
}

type readServicesMinimumStore struct {
	*store.FSStore
	requirement domain.MemoryReadRequirement
	observed    int
	reads       int
	demands     int
}

func (storage *readServicesMinimumStore) ObserveMemoryReadRequirement(ctx context.Context, _ domain.MemoryReadTarget, authorize func(context.Context) (context.Context, error)) (domain.MemoryReadRequirement, error) {
	storage.observed++
	if _, err := authorize(ctx); err != nil {
		return domain.MemoryReadRequirement{}, err
	}
	return storage.requirement, nil
}

func (storage *readServicesMinimumStore) WithMinimumMemoryReadRange(ctx context.Context, _ domain.MemoryReadTarget, _ domain.MemoryReadRequirement, _ domain.ContentHash,
	authorize func(context.Context) (context.Context, error), _ func(context.Context, domain.MemoryReadStatus, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	storage.reads++
	if _, err := authorize(ctx); err != nil {
		return err
	}
	return domain.ErrMemoryReadNotReady
}

func (storage *readServicesMinimumStore) RequestMinimumMemoryReadWork(context.Context, domain.MemoryReadTarget, domain.MemoryReadRequirement) (domain.MemoryReadWork, error) {
	storage.demands++
	return domain.MemoryReadWork{}, domain.ErrIntegrity
}

func TestReadServicesPreparedAllowlistCopiesAndNeverRequestsLocalDemand(test *testing.T) {
	config, filesystem, repo, document := readServicesFixture(test)
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: document.Hash, CodeCommit: strings.Repeat("a", 40)}}
	target, err := memoryquery.ReadTarget(repo, request)
	if err != nil {
		test.Fatal(err)
	}
	identifier, err := domain.MemoryReadTargetID(context.Background(), target)
	if err != nil {
		test.Fatal(err)
	}
	storage := &readServicesMinimumStore{FSStore: filesystem, requirement: domain.MemoryReadRequirement{
		Version: domain.MemoryReadRequirementVersion, TargetID: identifier,
		Minimum: domain.RepositoryMemorySourceGeneration{Version: domain.RepositoryMemorySourceVersion,
			Graph: domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope,
				RepoID: repo, Epoch: "12345678-1234-4321-9876-123456789abc", Sequence: 1},
			Epoch: "23456789-1234-4321-9876-123456789abc", Sequence: 1},
	}}
	config.Metadata, config.PreparedMemoryRepositories = storage, []domain.ContentHash{repo}
	services, err := NewReadServices(config)
	if err != nil {
		test.Fatal(err)
	}
	configured := services.PreparedMemory.(*memoryquery.PreparedQueries)
	if !reflect.ValueOf(configured).Elem().FieldByName("requester").IsNil() || reflect.ValueOf(configured).Elem().FieldByName("demands").Len() != 0 {
		test.Fatal("MCP prepared memory retained a local requester or demand allowlist")
	}
	foreign := domain.HashContent([]byte("not-allowlisted"))
	config.PreparedMemoryRepositories[0] = foreign
	for range 3 {
		_, err := services.PreparedMemory.QueryPreparedMemory(systemTestContext(), repo, request)
		var pending *domain.MemoryReadPending
		if !errors.As(err, &pending) || pending.Cursor == "" {
			test.Fatal("read-only prepared query lost its fixed-floor retry", err)
		}
		request.Cursor = pending.Cursor
	}
	request.Cursor = ""
	if _, err := services.PreparedMemory.QueryPreparedMemory(systemTestContext(), foreign, request); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("mutated composition allowlist changed the reader", err)
	}
	if storage.observed != 1 || storage.reads != 3 || storage.demands != 0 {
		test.Fatal("prepared retries observed another floor or submitted local work", storage.observed, storage.reads, storage.demands)
	}
}

type readServicesNilFields struct{ outbound.DocFindFieldsStore }
type readServicesNilDisplay struct{ outbound.DocDisplayReader }

type readServicesRootRequirementStore struct{ *store.FSStore }

func (storage *readServicesRootRequirementStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	metadata, err := storage.FSStore.GetRepo(ctx, repo)
	metadata.RequiredDocIdentity = domain.DocumentIdentityRootV1
	return metadata, err
}

func TestReadServicesPublicationOffDoesNotEraseSupportedRootReadPolicy(test *testing.T) {
	config, filesystem, repo, document := readServicesFixture(test)
	config.Metadata = &readServicesRootRequirementStore{FSStore: filesystem}
	config.SupportedDocumentIdentities = []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
	services, err := NewReadServices(config)
	if err != nil {
		test.Fatal(err)
	}
	ctx := inbound.WithDocumentIdentities(systemTestContext(), config.SupportedDocumentIdentities)
	page, err := services.Documents.ReadDocEvents(ctx, repo, document.Hash, "", 0, 1)
	if err != nil || len(page.Events) != 1 {
		test.Fatal("disabled publication erased retained root read support", page, err)
	}
	if _, err := services.Documents.ReadDocEvents(systemTestContext(), repo, document.Hash, "", 0, 1); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		test.Fatal("publication-off bypassed the repository's current identity requirement", err)
	}
}

type readServicesColdDocuments struct {
	*store.FSStore
	readinessChecks int
	demands         int
	wholeBodies     int
	readOnlyReads   int
}

func (storage *readServicesColdDocuments) HasDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	storage.readinessChecks++
	return false, nil
}

func (storage *readServicesColdDocuments) HasDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	storage.readinessChecks++
	return false, nil
}

func (*readServicesColdDocuments) LegacyByteRangePublicationEnabled() bool { return true }

func (storage *readServicesColdDocuments) HasDocByteRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	storage.readinessChecks++
	return false, nil
}

func (storage *readServicesColdDocuments) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	if !outbound.DocReadOnly(ctx) {
		return domain.DocReadIndex{}, domain.ErrForbidden
	}
	storage.readOnlyReads++
	return storage.FSStore.DocReadIndex(ctx, repo, hash)
}

func (storage *readServicesColdDocuments) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	storage.wholeBodies++
	return storage.FSStore.GetDoc(ctx, repo, hash)
}

func (storage *readServicesColdDocuments) EnsureDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) error {
	storage.demands++
	return domain.ErrIntegrity
}

func (storage *readServicesColdDocuments) EnqueueDocRangeJob(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	storage.demands++
	return domain.DocRangeIndexJob{}, domain.ErrIntegrity
}

func (storage *readServicesColdDocuments) EnqueueDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	storage.demands++
	return domain.DocRangeIndexJob{}, domain.ErrIntegrity
}

func (storage *readServicesColdDocuments) EnqueueDocByteRangeJob(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	storage.demands++
	return domain.DocRangeIndexJob{}, domain.ErrIntegrity
}

func (storage *readServicesColdDocuments) EnqueueDocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	storage.demands++
	return domain.DocRangeIndexJob{}, domain.ErrIntegrity
}

func (storage *readServicesColdDocuments) ReadDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int) (domain.DocFindFields, error) {
	return domain.DocFindFields{}, domain.ErrDocFindFieldsUnavailable
}

func (storage *readServicesColdDocuments) EnqueueDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	storage.demands++
	return domain.DocRangeIndexJob{}, domain.ErrIntegrity
}

func (*readServicesColdDocuments) ReadDocDisplayGeneration(context.Context, domain.DocDisplayTarget) (domain.DocDisplayStringGeneration, error) {
	return domain.DocDisplayStringGeneration{}, domain.ErrNotFound
}

func (*readServicesColdDocuments) ReadDocDisplayPage(context.Context, domain.DocDisplayTarget, int) (domain.DocDisplayPage, error) {
	return domain.DocDisplayPage{}, domain.ErrNotFound
}

func (*readServicesColdDocuments) ReadDocEventDisplayGeneration(context.Context, domain.DocDisplayTarget) (domain.DocDisplayProjectionGeneration, error) {
	return domain.DocDisplayProjectionGeneration{}, domain.ErrNotFound
}

func (*readServicesColdDocuments) ReadDocEventDisplayPage(context.Context, domain.DocDisplayTarget, int) (domain.DocDisplayPage, error) {
	return domain.DocDisplayPage{}, domain.ErrNotFound
}

func (storage *readServicesColdDocuments) EnqueueDocDisplay(context.Context, domain.DocDisplayTarget, time.Time) (domain.DocDisplayJob, error) {
	storage.demands++
	return domain.DocDisplayJob{}, domain.ErrIntegrity
}

func TestReadServicesDocumentReadsForceReadOnlyAndOmitEveryLocalDemandSlot(test *testing.T) {
	config, filesystem, repo, document := readServicesFixture(test)
	storage := &readServicesColdDocuments{FSStore: filesystem}
	config.Blobs, config.DocumentFindFields, config.DocumentDisplayReads = storage, storage, storage
	services, err := NewReadServices(config)
	if err != nil {
		test.Fatal(err)
	}
	queries := reflect.ValueOf(services.Documents).Elem()
	view, find := queries.FieldByName("view"), queries.FieldByName("find")
	for _, name := range []string{"RangeJobs", "VisiblePreparation", "LegacyPreparation", "Boundaries", "Bytes", "BytePreparation", "BoundaryPreparation"} {
		if field := view.FieldByName(name); field.IsValid() && !field.IsNil() {
			test.Fatal("standalone document queries retained a mixed or command dependency", name)
		}
	}
	for _, name := range []string{"Fields", "Display", "FieldsDemand"} {
		if field := find.FieldByName(name); field.IsValid() && !field.IsNil() {
			test.Fatal("standalone find queries retained a mixed or command dependency", name)
		}
	}
	if find.FieldByName("FieldsRead").Elem().Pointer() != reflect.ValueOf(storage).Pointer() ||
		find.FieldByName("DisplayRead").Elem().Pointer() != reflect.ValueOf(storage).Pointer() ||
		find.FieldByName("EventDisplay").Elem().Pointer() != reflect.ValueOf(storage).Pointer() {
		test.Fatal("find/display readers lost their explicit original bindings")
	}
	ctx := systemTestContext()
	if outbound.DocReadOnly(ctx) {
		test.Fatal("fixture hid a missing assembly read-only setting")
	}
	for _, operation := range []struct {
		name string
		read func() error
	}{
		{"events", func() error {
			_, err := services.Documents.ReadDocEvents(ctx, repo, document.Hash, "", 0, 1)
			return err
		}},
		{"fragments", func() error {
			_, err := services.Documents.ReadDocFragments(ctx, repo, document.Hash, 0, 0, 1, 4096)
			return err
		}},
		{"search", func() error {
			_, err := services.Documents.SearchDocEvents(ctx, repo, document.Hash, "needle", -1, 1)
			return err
		}},
		{"view", func() error {
			_, err := services.Documents.ReadDocViewEvents(ctx, repo, document.Hash, "", 0, 1, "all")
			return err
		}},
		{"view-fragment", func() error {
			_, err := services.Documents.ReadDocViewFragment(ctx, repo, document.Hash, 0, 0, 4096)
			return err
		}},
		{"visible", func() error {
			_, err := services.Documents.ReadDocVisiblePage(ctx, repo, document.Hash, domain.DocVisiblePageRequest{View: "all", Limit: 1})
			if !errors.Is(err, domain.ErrDocVisibleProjectionUnsupported) {
				test.Fatal("root-only visible read changed legacy meaning", err)
			}
			return err
		}},
		{"find", func() error {
			_, err := services.Documents.FindDocPage(ctx, repo, document.Hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", Total: 1, To: 1})
			return err
		}},
		{"prepared-find", func() error {
			_, err := services.Documents.FindDocPage(ctx, repo, document.Hash, domain.DocFindPageRequest{
				Legacy: true, Representation: domain.DocFindPreparedRepresentation, Query: "needle", Total: 1, To: 1})
			return err
		}},
		{"find-fragment", func() error {
			_, err := services.Documents.ReadDocFindFragment(ctx, repo, document.Hash, domain.DocFindFragmentRequest{Legacy: true, Total: 1, Budget: 4096})
			return err
		}},
		{"conversation", func() error {
			_, err := services.ConversationFind.QueryConversationFind(ctx, repo, document.Hash, domain.ConversationFindRequest{Query: "needle"})
			return err
		}},
	} {
		test.Run(operation.name, func(test *testing.T) {
			err := operation.read()
			if errors.Is(err, domain.ErrValidation) || errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrForbidden) || storage.demands != 0 {
				test.Fatal("public read bypassed read-only mode or submitted local demand", err, storage.demands)
			}
		})
	}
	if storage.readOnlyReads == 0 || storage.readinessChecks == 0 || storage.wholeBodies != 0 || storage.demands != 0 {
		test.Fatal("cold read used full-body fallback or failed to exercise bound readers", storage.readOnlyReads, storage.readinessChecks, storage.wholeBodies, storage.demands)
	}
}

func TestReadServicesRejectsInvalidCompositionBeforeReturningQueries(test *testing.T) {
	base, filesystem, repo, _ := readServicesFixture(test)
	for _, scenario := range []struct {
		name   string
		change func(*ReadServicesConfig)
		want   error
	}{
		{"document-profile", func(config *ReadServicesConfig) { config.DocumentProfile = 0 }, domain.ErrValidation},
		{"context-profile", func(config *ReadServicesConfig) { config.ContextProfile = 0 }, domain.ErrValidation},
		{"metadata", func(config *ReadServicesConfig) { config.Metadata = nil }, domain.ErrValidation},
		{"blobs", func(config *ReadServicesConfig) { config.Blobs = nil }, domain.ErrValidation},
		{"typed-nil-metadata", func(config *ReadServicesConfig) { config.Metadata = (*store.FSStore)(nil) }, domain.ErrValidation},
		{"typed-nil-blobs", func(config *ReadServicesConfig) { config.Blobs = (*store.FSStore)(nil) }, domain.ErrValidation},
		{"typed-nil-fields", func(config *ReadServicesConfig) { config.DocumentFindFields = (*readServicesNilFields)(nil) }, domain.ErrValidation},
		{"typed-nil-display", func(config *ReadServicesConfig) { config.DocumentDisplayReads = (*readServicesNilDisplay)(nil) }, domain.ErrValidation},
		{"identities-required", func(config *ReadServicesConfig) { config.SupportedDocumentIdentities = nil }, domain.ErrValidation},
		{"invalid-identity", func(config *ReadServicesConfig) {
			config.SupportedDocumentIdentities = []domain.DocumentIdentity{"unknown"}
		}, domain.ErrUnsupportedDocumentIdentity},
		{"duplicate-identity", func(config *ReadServicesConfig) {
			config.SupportedDocumentIdentities = []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityLegacy}
		}, domain.ErrValidation},
		{"missing-legacy", func(config *ReadServicesConfig) {
			config.SupportedDocumentIdentities = []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
		}, domain.ErrValidation},
		{"unsupported-root-claim", func(config *ReadServicesConfig) {
			config.Blobs = struct{ outbound.BlobStore }{filesystem}
			config.SupportedDocumentIdentities = []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
		}, domain.ErrUnsupportedDocumentIdentity},
		{"publication-without-root", func(config *ReadServicesConfig) {
			config.SupportedDocumentIdentities = []domain.DocumentIdentity{domain.DocumentIdentityLegacy}
			config.RootPublicationEnabled = true
		}, domain.ErrRootPublicationDisabled},
		{"publication-without-transactions", func(config *ReadServicesConfig) { config.RootPublicationEnabled = true }, domain.ErrRootPublicationDisabled},
		{"pinned-without-snapshots", func(config *ReadServicesConfig) { config.ContextProfile = inbound.ContextQueryPinnedSnapshots }, domain.ErrCatalogUnsupported},
		{"snapshots-without-presence", func(config *ReadServicesConfig) {
			config.Metadata = struct {
				outbound.MetadataStore
				documentQuerySnapshots
			}{filesystem, &readServicesSnapshotStore{FSStore: filesystem}}
			config.SupportedDocumentIdentities = []domain.DocumentIdentity{domain.DocumentIdentityLegacy}
		}, domain.ErrCatalogUnsupported},
		{"verified-without-snapshots", func(config *ReadServicesConfig) { config.DocumentProfile = documentquery.VerifiedRoots }, domain.ErrCatalogUnsupported},
		{"prepared-repository", func(config *ReadServicesConfig) { config.PreparedMemoryRepositories = []domain.ContentHash{"invalid"} }, domain.ErrIntegrity},
		{"prepared-limit", func(config *ReadServicesConfig) {
			config.PreparedMemoryRepositories = make([]domain.ContentHash, domain.MaxMemoryReadWorkerRepos+1)
		}, domain.ErrValidation},
		{"prepared-reader", func(config *ReadServicesConfig) {
			config.Metadata = struct{ outbound.MetadataStore }{filesystem}
			config.SupportedDocumentIdentities = []domain.DocumentIdentity{domain.DocumentIdentityLegacy}
			config.PreparedMemoryRepositories = []domain.ContentHash{repo}
		}, domain.ErrCatalogUnsupported},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			config := base
			scenario.change(&config)
			services, err := NewReadServices(config)
			if !errors.Is(err, scenario.want) || services != nil {
				test.Fatal("invalid composition returned usable readers", services, err, scenario.want)
			}
		})
	}
}
