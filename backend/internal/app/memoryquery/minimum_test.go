package memoryquery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestMinimumMemoryCursorIsCanonicalAndGenerationBound(test *testing.T) {
	requirement := domain.HashContent([]byte("request floor"))
	generation := domain.HashContent([]byte("qualified generation"))
	other := domain.HashContent([]byte("another generation"))
	cursor := minimumMemoryCursor{Version: 1, Requirement: requirement, Generation: generation, Continuation: "inner-page-cursor"}
	raw, err := json.Marshal(cursor)
	if err != nil {
		test.Fatal(err)
	}
	encode := func(value string) string { return base64.RawURLEncoding.EncodeToString([]byte(value)) }
	valid := encode(string(raw))
	for _, sample := range []struct {
		name        string
		encoded     string
		requirement domain.ContentHash
		generation  domain.ContentHash
		expected    error
	}{
		{"valid", valid, requirement, generation, nil},
		{"missing-generation", valid, requirement, "", domain.ErrValidation},
		{"wrong-generation", valid, requirement, other, domain.ErrEffectiveMemoryCursorStale},
		{"wrong-floor", valid, other, generation, domain.ErrEffectiveMemoryCursorStale},
		{"invalid-base64", "%%%", requirement, generation, domain.ErrValidation},
		{"oversized", strings.Repeat("a", 1025), requirement, generation, domain.ErrValidation},
		{"bad-json", encode("["), requirement, generation, domain.ErrValidation},
		{"null", encode("null"), requirement, generation, domain.ErrValidation},
		{"noncanonical", encode(string(raw) + " "), requirement, generation, domain.ErrValidation},
		{"unknown-field", encode(strings.TrimSuffix(string(raw), "}") + `,"ignored":true}`), requirement, generation, domain.ErrValidation},
		{"duplicate-field", encode(strings.TrimSuffix(string(raw), "}") + `,"version":1}`), requirement, generation, domain.ErrValidation},
		{"wrong-version", encode(strings.Replace(string(raw), `"version":1`, `"version":2`, 1)), requirement, generation, domain.ErrValidation},
		{"missing-inner", encode(strings.Replace(string(raw), "inner-page-cursor", "", 1)), requirement, generation, domain.ErrValidation},
	} {
		test.Run(sample.name, func(test *testing.T) {
			actual, err := minimumMemoryContinuation(sample.encoded, sample.requirement, sample.generation)
			if !errors.Is(err, sample.expected) || (err != nil && actual != "") || (err == nil && actual != cursor.Continuation) {
				test.Fatal(actual, err)
			}
		})
	}
	if actual, err := minimumMemoryContinuation("", requirement, ""); actual != "" || err != nil {
		test.Fatal("first page required a preexisting generation", actual, err)
	}
}

type minimumOwnerScopeKey struct{}
type minimumOwnerSnapshotKey struct{}
type minimumOwnerAuthorizedKey struct{}

type minimumOwnerScope struct {
	test            *testing.T
	repo            domain.ContentHash
	depth           int
	admissions      int
	snapshotCalls   int
	failure         error
	exitFailure     error
	snapshotFailure error
}

func (scope *minimumOwnerScope) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if repo != scope.repo || read == nil {
		return domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope.failure != nil {
		return scope.failure
	}
	if ctx.Value(minimumOwnerScopeKey{}) == scope && scope.depth > 0 {
		return read(ctx)
	}
	scope.admissions++
	scope.depth++
	defer func() { scope.depth-- }()
	if err := read(context.WithValue(ctx, minimumOwnerScopeKey{}, scope)); err != nil {
		return err
	}
	return scope.exitFailure
}

func (scope *minimumOwnerScope) AuthorizeSnapshot(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	scope.snapshotCalls++
	if repo != scope.repo || ctx.Value(minimumOwnerScopeKey{}) != scope || scope.depth == 0 || ctx.Value(minimumOwnerSnapshotKey{}) == nil {
		scope.test.Fatal("snapshot authorization did not run inside the store-owned current snapshot")
	}
	if scope.snapshotFailure != nil {
		return ctx, scope.snapshotFailure
	}
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, minimumOwnerAuthorizedKey{}, ctx.Value(minimumOwnerSnapshotKey{})), nil
}

type minimumOwnerRead struct {
	target      domain.MemoryReadTarget
	requirement domain.MemoryReadRequirement
	generation  domain.ContentHash
}

type minimumOwnerStore struct {
	test             *testing.T
	scope            *minimumOwnerScope
	requirement      domain.MemoryReadRequirement
	bundle           domain.MemoryReadBundle
	status           domain.MemoryReadStatus
	observed         []domain.MemoryReadTarget
	reads            []minimumOwnerRead
	chunks           []int
	observeFailure   error
	readFailure      error
	exitFailure      error
	requiredIdentity domain.DocumentIdentity
	beforeAuthorize  func()
	cancelChunk      context.CancelFunc
}

var _ outbound.MinimumMemoryReadStore = (*minimumOwnerStore)(nil)

func (storage *minimumOwnerStore) authorize(ctx context.Context, authorize func(context.Context) (context.Context, error)) (context.Context, error) {
	if ctx.Value(minimumOwnerScopeKey{}) != storage.scope || storage.scope.depth == 0 || authorize == nil {
		storage.test.Fatal("minimum reader bypassed the injected scope or snapshot callback")
	}
	if storage.beforeAuthorize != nil {
		storage.beforeAuthorize()
	}
	bound, err := authorize(context.WithValue(ctx, minimumOwnerSnapshotKey{}, storage))
	if err != nil {
		return bound, err
	}
	if bound.Value(minimumOwnerAuthorizedKey{}) != storage {
		storage.test.Fatal("store-owned snapshot authorization context was discarded")
	}
	return bound, outbound.CheckDocumentIdentityCompatibility(bound, storage.requiredIdentity)
}

func (storage *minimumOwnerStore) ObserveMemoryReadRequirement(ctx context.Context, target domain.MemoryReadTarget, authorize func(context.Context) (context.Context, error)) (domain.MemoryReadRequirement, error) {
	storage.observed = append(storage.observed, target)
	if _, err := storage.authorize(ctx, authorize); err != nil {
		return domain.MemoryReadRequirement{}, err
	}
	if storage.observeFailure != nil {
		return domain.MemoryReadRequirement{}, storage.observeFailure
	}
	return storage.requirement, nil
}

func (storage *minimumOwnerStore) WithMinimumMemoryReadRange(ctx context.Context, target domain.MemoryReadTarget, requirement domain.MemoryReadRequirement, generation domain.ContentHash, authorize func(context.Context) (context.Context, error), consume func(context.Context, domain.MemoryReadStatus, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	storage.reads = append(storage.reads, minimumOwnerRead{target: target, requirement: requirement, generation: generation})
	bound, err := storage.authorize(ctx, authorize)
	if err != nil {
		return err
	}
	if storage.readFailure != nil {
		return storage.readFailure
	}
	if err := consume(bound, storage.status, storage.bundle.Header, storage); err != nil {
		return err
	}
	return storage.exitFailure
}

func (storage *minimumOwnerStore) ReadChunk(ctx context.Context, ordinal int) ([]domain.EffectiveMemoryItem, error) {
	if ctx.Value(minimumOwnerAuthorizedKey{}) != storage || storage.scope.depth == 0 {
		storage.test.Fatal("chunk read escaped the current authorized snapshot")
	}
	storage.chunks = append(storage.chunks, ordinal)
	if storage.cancelChunk != nil {
		storage.cancelChunk()
	}
	if ordinal < 0 || ordinal >= len(storage.bundle.Chunks) {
		return nil, domain.ErrIntegrity
	}
	return domain.DecodeMemoryReadChunk(ctx, storage.bundle.Header, ordinal, storage.bundle.Chunks[ordinal].Bytes)
}

type minimumOwnerFixture struct {
	target      domain.MemoryReadTarget
	request     domain.EffectiveMemoryRequest
	requirement domain.MemoryReadRequirement
	bundle      domain.MemoryReadBundle
	status      domain.MemoryReadStatus
	items       []domain.EffectiveMemoryItem
}

func newMinimumOwnerFixture(test *testing.T, count int) (minimumOwnerFixture, *minimumOwnerScope, *minimumOwnerStore) {
	test.Helper()
	repo := domain.HashContent([]byte(test.Name()))
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{
		SnapshotID: domain.HashContent([]byte("selected snapshot")), CodeCommit: strings.Repeat("a", 40),
	}}
	target, err := ReadTarget(repo, request)
	if err != nil {
		test.Fatal(err)
	}
	identifier, err := domain.MemoryReadTargetID(context.Background(), target)
	if err != nil {
		test.Fatal(err)
	}
	source := domain.RepositoryMemorySourceGeneration{
		Version: domain.RepositoryMemorySourceVersion, Epoch: "23456789-1234-4321-9876-123456789abc", Sequence: 9007199254740995,
		Graph: domain.RepositoryViewSourceCheckpoint{
			Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope,
			RepoID: repo, Epoch: "12345678-1234-4321-9876-123456789abc", Sequence: 9007199254740993,
		},
	}
	items := make([]domain.EffectiveMemoryItem, count)
	for ordinal := range items {
		items[ordinal] = domain.EffectiveMemoryItem{
			ID: domain.HashContent(fmt.Appendf(nil, "item %d", ordinal)), SourceSnapshot: request.Selection.SnapshotID,
			Kind: "legacy_summary", Text: fmt.Sprintf("historical text %d", ordinal), Part: ordinal,
			TextHash:              domain.HashContent([]byte("complete historical text")),
			MemoryClaimAssessment: domain.MemoryClaimAssessment{State: "review", Reason: "untyped_historical_text"},
		}
	}
	header := domain.MemoryReadHeader{
		Version: domain.MemoryReadVersion, Rules: domain.MemoryReadRules, CaptureID: domain.HashContent([]byte("capture")),
		Source: source, Origin: "https://example.invalid/owner/repo",
		View: domain.EffectiveMemoryPage{
			Selection: request.Selection, StateHash: domain.HashContent([]byte("state")),
			LineageHash: domain.HashContent([]byte("lineage")), Total: count,
			Revision: domain.RepositoryRevision{Graph: 9007199254740997, Evidence: 9007199254740999},
		},
	}
	header.View.DeliveryStateHash, err = domain.EffectiveMemoryDeliveryStateHash(context.Background(), repo, header.Origin, header.View, items)
	if err != nil {
		test.Fatal(err)
	}
	bundle, err := domain.BuildMemoryReadBundle(context.Background(), header, items)
	if err != nil {
		test.Fatal(err)
	}
	requirement := domain.MemoryReadRequirement{Version: domain.MemoryReadRequirementVersion, TargetID: identifier, Minimum: source}
	status := domain.MemoryReadStatus{
		Version: domain.MemoryReadVersion, Rules: domain.MemoryReadRules, Generation: bundle.ID,
		Minimum: source, Observed: source, Applied: source, Complete: true, Freshness: "current",
	}
	scope := &minimumOwnerScope{test: test, repo: repo}
	storage := &minimumOwnerStore{test: test, scope: scope, requirement: requirement, bundle: bundle, status: status}
	return minimumOwnerFixture{target: target, request: request, requirement: requirement, bundle: bundle, status: status, items: items}, scope, storage
}

func newMinimumOwnerQueries(test *testing.T, scope Scope, storage outbound.MinimumMemoryReadStore) *MinimumQueries {
	test.Helper()
	queries, err := NewMinimum(MinimumConfig{Profile: Available, Scope: scope, Reader: storage})
	if err != nil {
		test.Fatal(err)
	}
	return queries
}

func encodeMinimumOwnerJSON(test *testing.T, value any) string {
	test.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func minimumOwnerContinuation(test *testing.T, fixture minimumOwnerFixture, offset int) string {
	test.Helper()
	raw, err := json.Marshal(fixture.requirement)
	if err != nil {
		test.Fatal(err)
	}
	return encodeMinimumOwnerJSON(test, minimumMemoryCursor{
		Version: 1, Requirement: domain.HashContent(raw), Generation: fixture.bundle.ID,
		Continuation: encodeMinimumOwnerJSON(test, Cursor{Version: 1, State: fixture.bundle.Header.View.StateHash, Index: offset}),
	})
}

func TestMinimumOwnerConstructorProfilesAndDependencies(test *testing.T) {
	fixture, scope, storage := newMinimumOwnerFixture(test, 1)
	var absentScope *minimumOwnerScope
	var absentReader *minimumOwnerStore
	for _, sample := range []struct {
		name   string
		config MinimumConfig
		want   error
	}{
		{name: "missing-profile", config: MinimumConfig{Scope: scope}, want: domain.ErrValidation},
		{name: "unknown-profile", config: MinimumConfig{Profile: Profile(255), Scope: scope}, want: domain.ErrValidation},
		{name: "missing-scope", config: MinimumConfig{Profile: Available, Reader: storage}, want: domain.ErrCatalogUnsupported},
		{name: "typed-nil-scope", config: MinimumConfig{Profile: Available, Scope: absentScope, Reader: storage}, want: domain.ErrCatalogUnsupported},
		{name: "unavailable-missing-scope", config: MinimumConfig{Profile: Unavailable}, want: domain.ErrCatalogUnsupported},
		{name: "available-without-reader", config: MinimumConfig{Profile: Available, Scope: scope}, want: domain.ErrCatalogUnsupported},
		{name: "typed-nil-reader", config: MinimumConfig{Profile: Available, Scope: scope, Reader: absentReader}, want: domain.ErrCatalogUnsupported},
		{name: "unavailable-with-reader", config: MinimumConfig{Profile: Unavailable, Scope: scope, Reader: storage}, want: domain.ErrCatalogUnsupported},
		{name: "unavailable-typed-nil-reader", config: MinimumConfig{Profile: Unavailable, Scope: scope, Reader: absentReader}, want: domain.ErrCatalogUnsupported},
		{name: "invalid-identity", config: MinimumConfig{Profile: Available, Scope: scope, Reader: storage, SupportedIdentities: []domain.DocumentIdentity{"unknown"}}, want: domain.ErrUnsupportedDocumentIdentity},
	} {
		test.Run(sample.name, func(test *testing.T) {
			queries, err := NewMinimum(sample.config)
			if !errors.Is(err, sample.want) || queries != nil {
				test.Fatal("invalid minimum configuration accepted", queries, err)
			}
		})
	}
	unavailable, err := NewMinimum(MinimumConfig{Profile: Unavailable, Scope: scope})
	if err != nil || unavailable.Available() {
		test.Fatal("explicit unavailable profile rejected", err)
	}
	for _, queries := range []*MinimumQueries{nil, {}, unavailable} {
		if queries.Available() {
			test.Fatal("absent minimum reader reported available")
		}
		if requirement, err := queries.ObserveMemoryReadRequirement(context.Background(), fixture.target.RepoID, fixture.request); !errors.Is(err, domain.ErrCatalogUnsupported) || requirement != (domain.MemoryReadRequirement{}) {
			test.Fatal("unavailable reader observed a floor", requirement, err)
		}
		if page, err := queries.QueryMinimumMemory(context.Background(), fixture.target.RepoID, fixture.request, fixture.requirement, ""); !errors.Is(err, domain.ErrCatalogUnsupported) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
			test.Fatal("unavailable reader returned data", page, err)
		}
	}
	if scope.admissions != 0 || len(storage.observed) != 0 || len(storage.reads) != 0 {
		test.Fatal("unavailable configuration reached a scope or store")
	}
}

func TestMinimumOwnerCopiesSupportedIdentities(test *testing.T) {
	fixture, scope, storage := newMinimumOwnerFixture(test, 1)
	storage.requiredIdentity = domain.DocumentIdentityRootV1
	supported := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
	queries, err := NewMinimum(MinimumConfig{Profile: Available, Scope: scope, Reader: storage, SupportedIdentities: supported})
	if err != nil {
		test.Fatal(err)
	}
	supported[0] = domain.DocumentIdentityLegacy
	ctx := inbound.WithDocumentIdentities(context.Background(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if requirement, err := queries.ObserveMemoryReadRequirement(ctx, fixture.target.RepoID, fixture.request); err != nil || requirement != fixture.requirement {
		test.Fatal("caller mutation changed frozen binary identities", requirement, err)
	}
	if _, err := queries.QueryMinimumMemory(ctx, fixture.target.RepoID, fixture.request, fixture.requirement, ""); err != nil {
		test.Fatal("read lost identity compatibility", err)
	}
	if _, err := queries.QueryMinimumMemory(context.Background(), fixture.target.RepoID, fixture.request, fixture.requirement, ""); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		test.Fatal("binary support replaced the peer declaration", err)
	}
	if scope.depth != 0 {
		test.Fatal("identity failure leaked a resource scope")
	}
}

func TestMinimumOwnerExecutesBoundedRangeWithStoreOwnedAuthority(test *testing.T) {
	fixture, scope, storage := newMinimumOwnerFixture(test, 130)
	queries := newMinimumOwnerQueries(test, scope, storage)
	request := fixture.request
	request.Content = "all"
	requirement, err := queries.ObserveMemoryReadRequirement(context.Background(), fixture.target.RepoID, request)
	if err != nil || requirement != fixture.requirement || !slices.Equal(storage.observed, []domain.MemoryReadTarget{fixture.target}) {
		test.Fatal("floor observation changed target or source", requirement, err)
	}
	request.Limit = 2
	page, err := queries.QueryMinimumMemory(context.Background(), fixture.target.RepoID, request, requirement, "")
	if err != nil || page.Status != fixture.status || page.Cursor != "" || !reflect.DeepEqual(page.Page.Items, fixture.items[:2]) || page.Page.NextCursor != minimumOwnerContinuation(test, fixture, 2) {
		test.Fatal("minimum page or exact cursor changed", page, err)
	}
	request.Cursor, request.Limit = minimumOwnerContinuation(test, fixture, 60), 20
	page, err = queries.QueryMinimumMemory(context.Background(), fixture.target.RepoID, request, requirement, fixture.bundle.ID)
	if err != nil || !reflect.DeepEqual(page.Page.Items, fixture.items[60:80]) || page.Page.NextCursor != minimumOwnerContinuation(test, fixture, 80) {
		test.Fatal("fixed generation range did not advance", page, err)
	}
	if scope.admissions != 3 || scope.snapshotCalls != 3 || scope.depth != 0 || !slices.Equal(storage.chunks, []int{0, 0, 1}) {
		test.Fatal("range escaped authority/scope or scanned the whole result", scope, storage.chunks)
	}
	if len(storage.reads) != 2 || storage.reads[0] != (minimumOwnerRead{target: fixture.target, requirement: requirement}) || storage.reads[1] != (minimumOwnerRead{target: fixture.target, requirement: requirement, generation: fixture.bundle.ID}) {
		test.Fatal("explicit read port lost minimum or generation", storage.reads)
	}
}

func TestMinimumOwnerValidatesBeforeScopeAndStore(test *testing.T) {
	fixture, scope, storage := newMinimumOwnerFixture(test, 1)
	queries := newMinimumOwnerQueries(test, scope, storage)
	continued := fixture.request
	continued.Cursor = "already-started"
	if _, err := queries.ObserveMemoryReadRequirement(context.Background(), fixture.target.RepoID, continued); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("continued read observed a new minimum", err)
	}
	for _, sample := range []struct {
		name   string
		mutate func(*domain.EffectiveMemoryRequest, *domain.MemoryReadRequirement, *domain.ContentHash)
		want   error
	}{
		{name: "missing-floor", mutate: func(_ *domain.EffectiveMemoryRequest, requirement *domain.MemoryReadRequirement, _ *domain.ContentHash) {
			*requirement = domain.MemoryReadRequirement{}
		}, want: domain.ErrValidation},
		{name: "selection", mutate: func(request *domain.EffectiveMemoryRequest, _ *domain.MemoryReadRequirement, _ *domain.ContentHash) {
			request.Selection.SnapshotID = domain.HashContent([]byte("other snapshot"))
		}, want: domain.ErrValidation},
		{name: "content", mutate: func(request *domain.EffectiveMemoryRequest, _ *domain.MemoryReadRequirement, _ *domain.ContentHash) {
			request.Content = "claims"
		}, want: domain.ErrValidation},
		{name: "limit", mutate: func(request *domain.EffectiveMemoryRequest, _ *domain.MemoryReadRequirement, _ *domain.ContentHash) {
			request.Limit = 51
		}, want: domain.ErrValidation},
		{name: "generation", mutate: func(_ *domain.EffectiveMemoryRequest, _ *domain.MemoryReadRequirement, generation *domain.ContentHash) {
			*generation = "invalid"
		}, want: domain.ErrIntegrity},
		{name: "inner-cursor", mutate: func(request *domain.EffectiveMemoryRequest, _ *domain.MemoryReadRequirement, _ *domain.ContentHash) {
			request.Cursor = "!"
		}, want: domain.ErrValidation},
		{name: "oversized-cursor", mutate: func(request *domain.EffectiveMemoryRequest, _ *domain.MemoryReadRequirement, _ *domain.ContentHash) {
			request.Cursor = strings.Repeat("a", 1025)
		}, want: domain.ErrValidation},
		{name: "changed-floor", mutate: func(request *domain.EffectiveMemoryRequest, requirement *domain.MemoryReadRequirement, generation *domain.ContentHash) {
			request.Cursor, *generation = minimumOwnerContinuation(test, fixture, 1), fixture.bundle.ID
			requirement.Minimum.Sequence++
		}, want: domain.ErrEffectiveMemoryCursorStale},
		{name: "changed-generation", mutate: func(request *domain.EffectiveMemoryRequest, _ *domain.MemoryReadRequirement, generation *domain.ContentHash) {
			request.Cursor, *generation = minimumOwnerContinuation(test, fixture, 1), domain.HashContent([]byte("new generation"))
		}, want: domain.ErrEffectiveMemoryCursorStale},
	} {
		test.Run(sample.name, func(test *testing.T) {
			request, requirement, generation := fixture.request, fixture.requirement, domain.ContentHash("")
			sample.mutate(&request, &requirement, &generation)
			page, err := queries.QueryMinimumMemory(context.Background(), fixture.target.RepoID, request, requirement, generation)
			if !errors.Is(err, sample.want) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
				test.Fatal("invalid minimum read returned data", page, err)
			}
		})
	}
	if scope.admissions != 0 || len(storage.observed) != 0 || len(storage.reads) != 0 {
		test.Fatal("invalid read reached scope or storage")
	}
}

func TestMinimumOwnerRejectsUnqualifiedOrMismatchedResult(test *testing.T) {
	for _, scenario := range []string{"incomplete", "status-version", "minimum", "applied", "generation", "selection", "content"} {
		test.Run(scenario, func(test *testing.T) {
			fixture, scope, storage := newMinimumOwnerFixture(test, 2)
			queries := newMinimumOwnerQueries(test, scope, storage)
			want := domain.ErrIntegrity
			switch scenario {
			case "incomplete":
				storage.status.Complete = false
			case "status-version":
				storage.status.Version++
			case "minimum":
				storage.status.Minimum.Sequence--
			case "applied":
				storage.bundle.Header.Source.Sequence++
			case "generation":
				storage.status.Generation = domain.HashContent([]byte("other generation"))
			case "selection":
				storage.bundle.Header.View.Selection.CodeCommit = strings.Repeat("b", 40)
				want = domain.ErrValidation
			case "content":
				storage.bundle.Header.View.Content = "claims"
				want = domain.ErrValidation
			}
			page, err := queries.QueryMinimumMemory(context.Background(), fixture.target.RepoID, fixture.request, fixture.requirement, fixture.bundle.ID)
			if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) || len(storage.chunks) != 0 || scope.depth != 0 {
				test.Fatal("unqualified/mismatched result served", page, err, storage.chunks)
			}
		})
	}
}

func TestMinimumOwnerPreservesObservationAndReadFailures(test *testing.T) {
	for _, scenario := range []string{"scope", "snapshot", "observe", "invalid-observation", "read", "read-close", "scope-close", "cancel-chunk"} {
		test.Run(scenario, func(test *testing.T) {
			fixture, scope, storage := newMinimumOwnerFixture(test, 2)
			queries := newMinimumOwnerQueries(test, scope, storage)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := errors.New("owner failure")
			observe := false
			switch scenario {
			case "scope":
				scope.failure = want
			case "snapshot":
				scope.snapshotFailure = want
			case "observe":
				storage.observeFailure, observe = want, true
			case "invalid-observation":
				storage.requirement.TargetID = domain.HashContent([]byte("foreign target"))
				want, observe = domain.ErrValidation, true
			case "read":
				storage.readFailure = want
			case "read-close":
				storage.exitFailure = want
			case "scope-close":
				scope.exitFailure = want
			case "cancel-chunk":
				storage.cancelChunk, want = cancel, context.Canceled
			}
			if observe {
				requirement, err := queries.ObserveMemoryReadRequirement(ctx, fixture.target.RepoID, fixture.request)
				if !errors.Is(err, want) || requirement != (domain.MemoryReadRequirement{}) {
					test.Fatal("observation failure exposed a floor", requirement, err)
				}
			} else {
				page, err := queries.QueryMinimumMemory(ctx, fixture.target.RepoID, fixture.request, fixture.requirement, "")
				if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
					test.Fatal("failed read exposed a partial page", page, err)
				}
			}
			if scope.depth != 0 {
				test.Fatal("failure leaked the injected scope")
			}
		})
	}
}

func TestMinimumOwnerRangeAdapterRejectsForeignScope(test *testing.T) {
	fixture, scope, storage := newMinimumOwnerFixture(test, 1)
	reader := &minimumMemoryRangeReader{scope: scope, storage: storage, target: fixture.target, requirement: fixture.requirement}
	for _, foreign := range []struct {
		repo     domain.ContentHash
		identity domain.ContentHash
	}{
		{repo: domain.HashContent([]byte("foreign repository")), identity: fixture.requirement.TargetID},
		{repo: fixture.target.RepoID, identity: domain.HashContent([]byte("foreign target"))},
	} {
		if err := reader.WithMemoryReadRange(context.Background(), foreign.repo, foreign.identity, nil); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("foreign minimum range accepted", err)
		}
	}
	if len(storage.reads) != 0 || scope.snapshotCalls != 0 {
		test.Fatal("foreign range reached store or authority")
	}
}
