package memoryquery

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type servingOwnerScope struct {
	*minimumOwnerScope
	snapshots           int
	snapshotDepth       int
	snapshotExitFailure error
}

func (scope *servingOwnerScope) WithinMemorySnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	scope.snapshots++
	bound, err := scope.AuthorizeSnapshot(context.WithValue(ctx, minimumOwnerSnapshotKey{}, scope), repo)
	if err != nil {
		return err
	}
	scope.snapshotDepth++
	defer func() { scope.snapshotDepth-- }()
	if err := read(bound); err != nil {
		return err
	}
	return scope.snapshotExitFailure
}

type servingOwnerStore struct {
	scope         *servingOwnerScope
	ranges        *minimumOwnerStore
	current       []domain.MemoryReadTarget
	qualified     []domain.ContentHash
	workReads     []domain.ContentHash
	work          domain.MemoryReadWork
	workFailure   error
	transaction   bool
	presenceCalls int
}

var _ ServingScope = (*servingOwnerScope)(nil)
var _ outbound.CurrentMemoryReadStore = (*servingOwnerStore)(nil)
var _ outbound.MemoryReadQualificationStore = (*servingOwnerStore)(nil)
var _ WorkStatusReader = (*servingOwnerStore)(nil)
var _ outbound.RepositoryTransactionPresence = (*servingOwnerStore)(nil)

func (storage *servingOwnerStore) WithCurrentMemoryReadRange(ctx context.Context, target domain.MemoryReadTarget, authorize func(context.Context) (context.Context, error), consume func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	storage.current = append(storage.current, target)
	identifier, err := domain.MemoryReadTargetID(ctx, target)
	if err != nil {
		return err
	}
	if identifier != storage.ranges.requirement.TargetID {
		return domain.ErrValidation
	}
	return storage.consume(ctx, authorize, consume)
}

func (storage *servingOwnerStore) WithQualifiedMemoryReadRange(ctx context.Context, repo, identity domain.ContentHash, authorize func(context.Context) (context.Context, error), consume func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	storage.qualified = append(storage.qualified, identity)
	if repo != storage.scope.repo || identity != storage.ranges.bundle.ID {
		return domain.ErrNotFound
	}
	return storage.consume(ctx, authorize, consume)
}

func (storage *servingOwnerStore) QualifyMemoryRead(context.Context, domain.MemoryReadQualification) (domain.ContentHash, error) {
	panic("serving query attempted publication")
}

func (storage *servingOwnerStore) consume(ctx context.Context, authorize func(context.Context) (context.Context, error), consume func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	bound, err := storage.ranges.authorize(ctx, authorize)
	if err != nil {
		return err
	}
	if storage.ranges.readFailure != nil {
		return storage.ranges.readFailure
	}
	if err := consume(bound, storage.ranges.bundle.Header, storage.ranges); err != nil {
		return err
	}
	return storage.ranges.exitFailure
}

func (storage *servingOwnerStore) ReadMemoryReadWork(ctx context.Context, repo, identity domain.ContentHash) (domain.MemoryReadWork, error) {
	storage.workReads = append(storage.workReads, identity)
	if repo != storage.scope.repo || storage.scope.depth == 0 || storage.scope.snapshotDepth == 0 ||
		ctx.Value(minimumOwnerAuthorizedKey{}) != storage.scope || identity != storage.work.ID {
		storage.scope.test.Fatal("work status escaped the injected repository snapshot")
	}
	return storage.work, storage.workFailure
}

func (storage *servingOwnerStore) InTransaction(context.Context) bool {
	storage.presenceCalls++
	return storage.transaction
}

func newServingOwnerFixture(test *testing.T, count int) (minimumOwnerFixture, *servingOwnerScope, *servingOwnerStore) {
	test.Helper()
	fixture, resources, ranges := newMinimumOwnerFixture(test, count)
	scope := &servingOwnerScope{minimumOwnerScope: resources}
	storage := &servingOwnerStore{scope: scope, ranges: ranges,
		work: domain.MemoryReadWork{ID: fixture.requirement.TargetID, Target: fixture.target, Source: fixture.bundle.Header.Source, State: "waiting", Version: 1}}
	return fixture, scope, storage
}

func newServingOwnerQueries(test *testing.T, scope ServingScope, storage *servingOwnerStore) *ServingQueries {
	test.Helper()
	queries, err := NewServing(ServingConfig{Scope: scope, Current: storage, Qualified: storage, WorkStatus: storage, Transactions: storage})
	if err != nil {
		test.Fatal(err)
	}
	return queries
}

func servingOwnerRead(queries *ServingQueries, operation string, ctx context.Context, fixture minimumOwnerFixture, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	if operation == "current" {
		return queries.QueryCurrentMemory(ctx, fixture.target.RepoID, request)
	}
	return queries.QueryQualifiedMemory(ctx, fixture.target.RepoID, fixture.bundle.ID, request)
}

func TestServingConstructorExplicitOptionalCapabilities(test *testing.T) {
	fixture, scope, storage := newServingOwnerFixture(test, 1)
	var missingScope *servingOwnerScope
	var missingStore *servingOwnerStore
	for _, sample := range []struct {
		name   string
		config ServingConfig
		want   error
	}{
		{name: "missing-scope", want: domain.ErrCatalogUnsupported},
		{name: "typed-nil-scope", config: ServingConfig{Scope: missingScope}, want: domain.ErrCatalogUnsupported},
		{name: "typed-nil-current", config: ServingConfig{Scope: scope, Current: missingStore}, want: domain.ErrCatalogUnsupported},
		{name: "typed-nil-qualified", config: ServingConfig{Scope: scope, Qualified: missingStore}, want: domain.ErrCatalogUnsupported},
		{name: "typed-nil-status", config: ServingConfig{Scope: scope, WorkStatus: missingStore}, want: domain.ErrCatalogUnsupported},
		{name: "typed-nil-transactions", config: ServingConfig{Scope: scope, Transactions: missingStore}, want: domain.ErrCatalogUnsupported},
		{name: "invalid-identity", config: ServingConfig{Scope: scope, SupportedIdentities: []domain.DocumentIdentity{"unknown"}}, want: domain.ErrUnsupportedDocumentIdentity},
	} {
		test.Run(sample.name, func(test *testing.T) {
			queries, err := NewServing(sample.config)
			if !errors.Is(err, sample.want) || queries != nil {
				test.Fatal("invalid serving configuration accepted", queries, err)
			}
		})
	}
	unavailable, err := NewServing(ServingConfig{Scope: scope})
	if err != nil {
		test.Fatal("optional capability absence prevented construction", err)
	}
	for _, operation := range []string{"current", "qualified"} {
		page, err := servingOwnerRead(unavailable, operation, context.Background(), fixture, fixture.request)
		if !errors.Is(err, domain.ErrCatalogUnsupported) || !reflect.ValueOf(page).IsZero() {
			test.Fatal("unavailable range did not preserve unsupported", operation, page, err)
		}
	}
	work, err := unavailable.ReadMemoryPreparationStatus(inbound.WithSystemActor(context.Background()), fixture.target.RepoID, fixture.request)
	if !errors.Is(err, domain.ErrCatalogUnsupported) || !reflect.ValueOf(work).IsZero() || scope.admissions != 0 {
		test.Fatal("unavailable status entered resources", work, err)
	}
	for _, operation := range []string{"current", "qualified"} {
		config := ServingConfig{Scope: scope}
		if operation == "current" {
			config.Current = storage
		} else {
			config.Qualified = storage
		}
		queries, err := NewServing(config)
		if err != nil {
			test.Fatal(err)
		}
		if page, err := servingOwnerRead(queries, operation, context.Background(), fixture, fixture.request); err != nil || len(page.Items) != 1 {
			test.Fatal("independent range required unrelated optional ports", operation, page, err)
		}
	}
	if len(storage.workReads) != 0 || storage.presenceCalls != 0 {
		test.Fatal("range query rediscovered status or transaction capabilities")
	}
}

func TestServingSnapshotCallbacksPreserveHashesAndBounds(test *testing.T) {
	for _, operation := range []string{"current", "qualified"} {
		for _, sample := range []struct {
			name   string
			limit  int
			offset int
			count  int
			reads  []int
		}{
			{name: "default", count: 20, reads: []int{0}},
			{name: "maximum", limit: 50, count: 50, reads: []int{0}},
			{name: "chunk-boundary", limit: 20, offset: 60, count: 20, reads: []int{0, 1}},
			{name: "end", limit: 20, offset: 130},
		} {
			test.Run(operation+"/"+sample.name, func(test *testing.T) {
				fixture, scope, storage := newServingOwnerFixture(test, 130)
				queries := newServingOwnerQueries(test, scope, storage)
				request := fixture.request
				request.Content, request.Limit = "all", sample.limit
				if sample.offset != 0 {
					request.Cursor = rangePageCursor(test, fixture.bundle.Header.View.StateHash, sample.offset)
				}
				page, err := servingOwnerRead(queries, operation, context.Background(), fixture, request)
				want := fixture.bundle.Header.View
				want.Items = append([]domain.EffectiveMemoryItem{}, fixture.items[sample.offset:sample.offset+sample.count]...)
				if sample.offset+sample.count < want.Total {
					want.NextCursor = rangePageCursor(test, want.StateHash, sample.offset+sample.count)
				}
				if err != nil || !reflect.DeepEqual(page, want) || !slices.Equal(storage.ranges.chunks, sample.reads) {
					test.Fatal("serving changed hashes, selected items, or bounded chunk reads", page, storage.ranges.chunks, err)
				}
				if scope.admissions != 1 || scope.snapshotCalls != 1 || scope.snapshots != 0 || scope.depth != 0 || storage.presenceCalls != 0 {
					test.Fatal("range escaped store-owned snapshot authorization or resource lifetime", scope, storage.presenceCalls)
				}
				if operation == "current" && !reflect.DeepEqual(storage.current, []domain.MemoryReadTarget{fixture.target}) ||
					operation == "qualified" && !slices.Equal(storage.qualified, []domain.ContentHash{fixture.bundle.ID}) {
					test.Fatal("range changed normalized target or qualification identity", storage.current, storage.qualified)
				}
			})
		}
	}
}

func TestServingRangeByteBoundUsesSharedReadPage(test *testing.T) {
	for _, operation := range []string{"current", "qualified"} {
		test.Run(operation, func(test *testing.T) {
			fixture, scope, storage := newServingOwnerFixture(test, 0)
			items := []domain.EffectiveMemoryItem{
				rangePageSizedItem(test, fixture.request.Selection.SnapshotID, 0, MaxPageBytes/2),
				rangePageSizedItem(test, fixture.request.Selection.SnapshotID, 1, MaxPageBytes/2),
				rangePageSizedItem(test, fixture.request.Selection.SnapshotID, 2, 1024),
			}
			fixture.bundle = rangePageBuild(test, fixture.bundle.Header, items)
			storage.ranges.bundle = fixture.bundle
			queries := newServingOwnerQueries(test, scope, storage)
			request := fixture.request
			request.Limit = 50
			page, err := servingOwnerRead(queries, operation, context.Background(), fixture, request)
			if err != nil || !reflect.DeepEqual(page.Items, items[:2]) || !slices.Equal(storage.ranges.chunks, []int{0, 1}) ||
				page.NextCursor != rangePageCursor(test, fixture.bundle.Header.View.StateHash, 2) {
				test.Fatal("serving bypassed the shared serialized page bound", page, storage.ranges.chunks, err)
			}
		})
	}
}

func TestServingRangeErrorsDiscardResults(test *testing.T) {
	for _, operation := range []string{"current", "qualified"} {
		for _, scenario := range []string{"resource", "snapshot-revoked", "read", "chunk-canceled", "range-exit", "resource-exit", "cursor", "stale", "past-end"} {
			test.Run(operation+"/"+scenario, func(test *testing.T) {
				fixture, scope, storage := newServingOwnerFixture(test, 130)
				queries := newServingOwnerQueries(test, scope, storage)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				request := fixture.request
				request.Limit, request.Cursor = 20, rangePageCursor(test, fixture.bundle.Header.View.StateHash, 60)
				want := domain.ErrValidation
				var reads []int
				switch scenario {
				case "resource":
					want, scope.failure = domain.ErrCatalogUnsupported, domain.ErrCatalogUnsupported
				case "snapshot-revoked":
					want = domain.ErrForbidden
					storage.ranges.beforeAuthorize = func() { scope.snapshotFailure = domain.ErrForbidden }
				case "read":
					want, storage.ranges.readFailure = domain.ErrConflict, domain.ErrConflict
				case "chunk-canceled":
					want, storage.ranges.cancelChunk, reads = context.Canceled, cancel, []int{0}
				case "range-exit":
					want, storage.ranges.exitFailure, reads = domain.ErrIntegrity, domain.ErrIntegrity, []int{0, 1}
				case "resource-exit":
					want, scope.exitFailure, reads = context.DeadlineExceeded, context.DeadlineExceeded, []int{0, 1}
				case "cursor":
					request.Cursor = "!"
				case "stale":
					request.Cursor = rangePageCursor(test, rangePageHash("other state"), 60)
					want = domain.ErrEffectiveMemoryCursorStale
				case "past-end":
					request.Cursor = rangePageCursor(test, fixture.bundle.Header.View.StateHash, 131)
				}
				page, err := servingOwnerRead(queries, operation, ctx, fixture, request)
				if !errors.Is(err, want) || !reflect.ValueOf(page).IsZero() || !slices.Equal(storage.ranges.chunks, reads) || scope.depth != 0 {
					test.Fatal("failed serving range leaked output or continued reading", page, storage.ranges.chunks, err)
				}
				if scenario == "resource" && scope.snapshotCalls != 0 || scenario != "resource" && scope.snapshotCalls != 1 {
					test.Fatal("source snapshot authorization callback changed", scope.snapshotCalls)
				}
			})
		}
	}
}

func TestServingRangeValidationPrecedence(test *testing.T) {
	for _, operation := range []string{"current", "qualified"} {
		for _, scenario := range []string{"negative-limit", "large-limit", "long-cursor", "content", "selection", "canceled"} {
			test.Run(operation+"/"+scenario, func(test *testing.T) {
				fixture, scope, storage := newServingOwnerFixture(test, 1)
				queries := newServingOwnerQueries(test, scope, storage)
				request := fixture.request
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := domain.ErrValidation
				switch scenario {
				case "negative-limit":
					request.Limit = -1
				case "large-limit":
					request.Limit = 51
				case "long-cursor":
					request.Cursor = strings.Repeat("x", 1025)
				case "content":
					request.Content = "invalid"
				case "selection":
					request.Selection.CodeCommit = "invalid"
				case "canceled":
					cancel()
					want = context.Canceled
				}
				page, err := servingOwnerRead(queries, operation, ctx, fixture, request)
				if !errors.Is(err, want) || !reflect.ValueOf(page).IsZero() || len(storage.current)+len(storage.qualified) != 0 {
					test.Fatal("invalid request opened a serving range", operation, page, err)
				}
				if operation == "current" && scope.admissions != 0 || operation == "qualified" && scenario != "canceled" && scope.admissions != 1 {
					test.Fatal("current validation or qualified admission order changed", scope.admissions)
				}
			})
		}
	}
	fixture, scope, _ := newServingOwnerFixture(test, 1)
	unavailable, err := NewServing(ServingConfig{Scope: scope})
	if err != nil {
		test.Fatal(err)
	}
	invalid := fixture.request
	invalid.Limit = -1
	if _, err := unavailable.QueryCurrentMemory(context.Background(), fixture.target.RepoID, invalid); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("current capability absence overtook target validation", err)
	}
	if _, err := unavailable.QueryQualifiedMemory(context.Background(), fixture.target.RepoID, fixture.bundle.ID, invalid); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("qualified validation overtook capability absence", err)
	}
}

func TestServingCurrentRangeRejectsForeignTargetBeforeStore(test *testing.T) {
	for _, scenario := range []string{"repository", "identity", "target", "canceled"} {
		test.Run(scenario, func(test *testing.T) {
			fixture, scope, storage := newServingOwnerFixture(test, 1)
			reader := currentMemoryRangeReader{scope: scope, storage: storage, target: fixture.target}
			repo, identifier := fixture.target.RepoID, fixture.requirement.TargetID
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := domain.ErrValidation
			switch scenario {
			case "repository":
				repo = rangePageHash("foreign repository")
			case "identity":
				identifier = rangePageHash("foreign target")
			case "target":
				reader.target.Version = 0
			case "canceled":
				cancel()
				want = context.Canceled
			}
			err := reader.WithMemoryReadRange(ctx, repo, identifier, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
				test.Fatal("foreign target entered the range callback")
				return nil
			})
			if !errors.Is(err, want) || len(storage.current) != 0 || scope.snapshotCalls != 0 {
				test.Fatal("foreign target reached current storage", err)
			}
		})
	}
}

func TestServingIdentitiesAreCopiedAndReachPinnedRead(test *testing.T) {
	for _, operation := range []string{"current", "qualified"} {
		test.Run(operation, func(test *testing.T) {
			fixture, scope, storage := newServingOwnerFixture(test, 1)
			storage.ranges.requiredIdentity = domain.DocumentIdentityRootV1
			supported := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
			queries, err := NewServing(ServingConfig{Scope: scope, Current: storage, Qualified: storage, SupportedIdentities: supported})
			if err != nil {
				test.Fatal(err)
			}
			supported[0] = domain.DocumentIdentityLegacy
			ctx := inbound.WithDocumentIdentities(context.Background(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
			if page, err := servingOwnerRead(queries, operation, ctx, fixture, fixture.request); err != nil || len(page.Items) != 1 {
				test.Fatal("constructor identities aliased caller state or lost the pinned context", page, err)
			}
			storage.ranges.chunks = nil
			page, err := servingOwnerRead(queries, operation, context.Background(), fixture, fixture.request)
			if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || !reflect.ValueOf(page).IsZero() || len(storage.ranges.chunks) != 0 {
				test.Fatal("missing peer identity escaped the source snapshot gate", page, err)
			}
		})
	}
}

func TestServingStatusAuthorityAndTransactionPrecedence(test *testing.T) {
	for _, scenario := range []string{"anonymous", "user", "general-authorizer", "unknown-transaction", "borrowed-transaction", "invalid-target", "canceled", "missing-status"} {
		test.Run(scenario, func(test *testing.T) {
			fixture, scope, storage := newServingOwnerFixture(test, 1)
			config := ServingConfig{Scope: scope, WorkStatus: storage, Transactions: storage}
			ctx, cancel := context.WithCancel(inbound.WithSystemActor(context.Background()))
			defer cancel()
			request, repo := fixture.request, fixture.target.RepoID
			want, presence := domain.ErrForbidden, 0
			switch scenario {
			case "anonymous":
				ctx = context.Background()
				config.Transactions = nil
			case "user":
				ctx = inbound.WithRepositoryActor(context.Background(), "user")
			case "general-authorizer":
				ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
					test.Fatal("system-only status invoked transport authorization", requested)
					return bound, nil
				})
			case "unknown-transaction":
				config.Transactions, repo = nil, "invalid"
				want = domain.ErrCatalogUnsupported
			case "borrowed-transaction":
				storage.transaction, repo = true, "invalid"
				want, presence = domain.ErrConflict, 1
			case "invalid-target":
				config.WorkStatus, request.Limit = nil, -1
				want, presence = domain.ErrValidation, 1
			case "canceled":
				config.WorkStatus = nil
				cancel()
				want, presence = context.Canceled, 1
			case "missing-status":
				config.WorkStatus = nil
				want, presence = domain.ErrCatalogUnsupported, 1
			}
			queries, err := NewServing(config)
			if err != nil {
				test.Fatal(err)
			}
			work, err := queries.ReadMemoryPreparationStatus(ctx, repo, request)
			if !errors.Is(err, want) || !reflect.ValueOf(work).IsZero() || storage.presenceCalls != presence ||
				scope.admissions != 0 || scope.snapshots != 0 || len(storage.workReads) != 0 {
				test.Fatal("status validation, authority, or transaction precedence changed", work, storage.presenceCalls, err)
			}
		})
	}
}

func TestServingStatusIsReadOnlyAndSnapshotBound(test *testing.T) {
	fixture, scope, storage := newServingOwnerFixture(test, 1)
	queries, err := NewServing(ServingConfig{Scope: scope, WorkStatus: storage, Transactions: storage})
	if err != nil {
		test.Fatal(err)
	}
	request := fixture.request
	request.Content, request.Limit, request.Cursor = "all", 50, "existing-continuation"
	work, err := queries.ReadMemoryPreparationStatus(inbound.WithSystemActor(context.Background()), fixture.target.RepoID, request)
	if err != nil || !reflect.DeepEqual(work, storage.work) || !slices.Equal(storage.workReads, []domain.ContentHash{fixture.requirement.TargetID}) {
		test.Fatal("status changed durable target normalization or returned work", work, storage.workReads, err)
	}
	if scope.admissions != 1 || scope.snapshots != 1 || scope.snapshotCalls != 1 || scope.depth != 0 || scope.snapshotDepth != 0 ||
		storage.presenceCalls != 1 || len(storage.current)+len(storage.qualified)+len(storage.ranges.chunks) != 0 {
		test.Fatal("status did not stay inside the independent read-only snapshot", scope, storage)
	}
}

func TestServingStatusFailuresDiscardWork(test *testing.T) {
	for _, scenario := range []string{"resource", "snapshot", "work", "snapshot-exit", "resource-exit"} {
		test.Run(scenario, func(test *testing.T) {
			fixture, scope, storage := newServingOwnerFixture(test, 1)
			queries := newServingOwnerQueries(test, scope, storage)
			want := errors.New("status scope failure")
			reads, snapshots := 1, 1
			switch scenario {
			case "resource":
				scope.failure, reads, snapshots = want, 0, 0
			case "snapshot":
				scope.snapshotFailure, reads = want, 0
			case "work":
				storage.workFailure = want
			case "snapshot-exit":
				scope.snapshotExitFailure = want
			case "resource-exit":
				scope.exitFailure = want
			}
			work, err := queries.ReadMemoryPreparationStatus(inbound.WithSystemActor(context.Background()), fixture.target.RepoID, fixture.request)
			if !errors.Is(err, want) || !reflect.ValueOf(work).IsZero() || len(storage.workReads) != reads ||
				scope.snapshots != snapshots || scope.depth != 0 || scope.snapshotDepth != 0 {
				test.Fatal("status failure leaked partially read work or snapshot resources", work, err)
			}
		})
	}
}

type servingAuthorizationKey struct{}

func TestServingSnapshotAuthorizationPrimitive(test *testing.T) {
	repo := rangePageHash("authorized repository")
	for _, scenario := range []string{"anonymous", "user", "system", "general-only", "system-general", "snapshot", "system-snapshot-denied", "snapshot-with-general"} {
		test.Run(scenario, func(test *testing.T) {
			ctx := context.Background()
			want, calls := domain.ErrCatalogUnsupported, 0
			general := func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
				test.Fatal("snapshot authorization fell back to an admission callback", requested)
				return bound, nil
			}
			if strings.HasPrefix(scenario, "system") {
				ctx = inbound.WithSystemActor(ctx)
			}
			if scenario == "user" {
				ctx = inbound.WithRepositoryActor(ctx, "user")
			}
			if scenario == "general-only" || scenario == "system-general" || scenario == "snapshot-with-general" {
				ctx = inbound.WithDocumentReadAuthorizer(ctx, general)
			}
			if scenario == "system" {
				want = nil
			}
			if scenario == "snapshot" || scenario == "system-snapshot-denied" || scenario == "snapshot-with-general" {
				want = nil
				if scenario == "system-snapshot-denied" {
					want = domain.ErrForbidden
				}
				ctx = inbound.WithDocumentSnapshotReadAuthorizer(ctx, func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
					calls++
					if bound != ctx || requested != repo {
						test.Fatal("snapshot authorization changed source context or repository")
					}
					return context.WithValue(bound, servingAuthorizationKey{}, repo), want
				})
			}
			bound, err := AuthorizeSnapshot(ctx, repo)
			if !errors.Is(err, want) {
				test.Fatal("snapshot authority precedence changed", err)
			}
			if strings.Contains(scenario, "snapshot") {
				if calls != 1 || bound.Value(servingAuthorizationKey{}) != repo {
					test.Fatal("snapshot callback result context was discarded", calls, bound)
				}
			} else if calls != 0 || bound != ctx {
				test.Fatal("system fallback changed context or invoked a callback", calls, bound)
			}
		})
	}
}
