package memoryquery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type preparedOwnerRequester struct {
	test    *testing.T
	scope   *minimumOwnerScope
	calls   []minimumOwnerRead
	failure error
}

func (requester *preparedOwnerRequester) RequestPrepared(ctx context.Context, target domain.MemoryReadTarget, requirement domain.MemoryReadRequirement) error {
	if ctx.Value(minimumOwnerScopeKey{}) != requester.scope || requester.scope.depth == 0 || ctx.Value(minimumOwnerSnapshotKey{}) != nil {
		requester.test.Fatal("preparation did not use the injected scope outside the read-only snapshot")
	}
	requester.calls = append(requester.calls, minimumOwnerRead{target: target, requirement: requirement})
	return requester.failure
}

func newPreparedOwnerQueries(test *testing.T, fixture minimumOwnerFixture, scope Scope, minimum *MinimumQueries, requester PreparationRequester, demand bool) *PreparedQueries {
	test.Helper()
	config := PreparedConfig{Scope: scope, Minimum: minimum, Repositories: []domain.ContentHash{fixture.target.RepoID}, Requester: requester}
	if demand {
		config.DemandRepositories = []domain.ContentHash{fixture.target.RepoID}
	}
	queries, err := NewPrepared(config)
	if err != nil {
		test.Fatal(err)
	}
	return queries
}

func TestPreparedOwnerConstructorRequiresActiveCapabilities(test *testing.T) {
	fixture, scope, storage := newMinimumOwnerFixture(test, 1)
	minimum := newMinimumOwnerQueries(test, scope, storage)
	unavailable, err := NewMinimum(MinimumConfig{Profile: Unavailable, Scope: scope})
	if err != nil {
		test.Fatal(err)
	}
	requester := &preparedOwnerRequester{test: test, scope: scope}
	var absentScope *minimumOwnerScope
	var absentRequester *preparedOwnerRequester
	for _, sample := range []struct {
		name   string
		config PreparedConfig
		want   error
	}{
		{name: "missing-scope", config: PreparedConfig{}, want: domain.ErrCatalogUnsupported},
		{name: "typed-nil-scope", config: PreparedConfig{Scope: absentScope}, want: domain.ErrCatalogUnsupported},
		{name: "missing-minimum", config: PreparedConfig{Scope: scope, Repositories: []domain.ContentHash{fixture.target.RepoID}}, want: domain.ErrCatalogUnsupported},
		{name: "unavailable-minimum", config: PreparedConfig{Scope: scope, Minimum: unavailable, Repositories: []domain.ContentHash{fixture.target.RepoID}}, want: domain.ErrCatalogUnsupported},
		{name: "typed-nil-requester", config: PreparedConfig{Scope: scope, Requester: absentRequester}, want: domain.ErrCatalogUnsupported},
		{name: "demand-without-requester", config: PreparedConfig{Scope: scope, Minimum: minimum, Repositories: []domain.ContentHash{fixture.target.RepoID}, DemandRepositories: []domain.ContentHash{fixture.target.RepoID}}, want: domain.ErrCatalogUnsupported},
		{name: "repository-limit", config: PreparedConfig{Scope: scope, Minimum: minimum, Repositories: make([]domain.ContentHash, domain.MaxMemoryReadWorkerRepos+1)}, want: domain.ErrValidation},
		{name: "demand-limit", config: PreparedConfig{Scope: scope, Requester: requester, DemandRepositories: make([]domain.ContentHash, domain.MaxMemoryReadWorkerRepos+1)}, want: domain.ErrValidation},
		{name: "invalid-repository", config: PreparedConfig{Scope: scope, Minimum: minimum, Repositories: []domain.ContentHash{"invalid"}}, want: domain.ErrIntegrity},
		{name: "invalid-demand", config: PreparedConfig{Scope: scope, Requester: requester, DemandRepositories: []domain.ContentHash{"invalid"}}, want: domain.ErrIntegrity},
	} {
		test.Run(sample.name, func(test *testing.T) {
			queries, err := NewPrepared(sample.config)
			if !errors.Is(err, sample.want) || queries != nil {
				test.Fatal("invalid prepared configuration accepted", queries, err)
			}
		})
	}
	disabled, err := NewPrepared(PreparedConfig{Scope: scope})
	if err != nil {
		test.Fatal("unused minimum/requester were required", err)
	}
	for _, queries := range []*PreparedQueries{nil, {}, disabled} {
		page, err := queries.QueryPreparedMemory(context.Background(), fixture.target.RepoID, fixture.request)
		if !errors.Is(err, domain.ErrCatalogUnsupported) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
			test.Fatal("disabled prepared reader executed", page, err)
		}
	}
	if scope.admissions != 0 || len(storage.observed) != 0 || len(storage.reads) != 0 || len(requester.calls) != 0 {
		test.Fatal("constructor or disabled query reached operational ports")
	}
}

func TestPreparedOwnerCopiesRepositoryAndDemandAllowlists(test *testing.T) {
	fixture, scope, storage := newMinimumOwnerFixture(test, 1)
	minimum := newMinimumOwnerQueries(test, scope, storage)
	requester := &preparedOwnerRequester{test: test, scope: scope}
	repositories := []domain.ContentHash{fixture.target.RepoID}
	demands := []domain.ContentHash{fixture.target.RepoID}
	queries, err := NewPrepared(PreparedConfig{Scope: scope, Minimum: minimum, Repositories: repositories, DemandRepositories: demands, Requester: requester})
	if err != nil {
		test.Fatal(err)
	}
	foreign := domain.HashContent([]byte("foreign repository"))
	repositories[0], demands[0] = foreign, foreign
	storage.readFailure = domain.ErrMemoryReadNotReady
	page, err := queries.QueryPreparedMemory(context.Background(), fixture.target.RepoID, fixture.request)
	var pending *domain.MemoryReadPending
	if !errors.As(err, &pending) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) || len(requester.calls) != 1 || requester.calls[0] != (minimumOwnerRead{target: fixture.target, requirement: fixture.requirement}) {
		test.Fatal("caller mutation changed a frozen allowlist", page, err, requester.calls)
	}
	page, err = queries.QueryPreparedMemory(context.Background(), foreign, fixture.request)
	if !errors.Is(err, domain.ErrCatalogUnsupported) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) || len(storage.reads) != 1 || len(storage.observed) != 1 || len(requester.calls) != 1 {
		test.Fatal("caller mutation enabled a foreign repository", page, err)
	}
}

func TestPreparedOwnerRequestsOnlyConfiguredPendingDemand(test *testing.T) {
	for _, scenario := range []string{"no-demand", "foreign-demand", "configured-demand", "ready", "demand-denied"} {
		test.Run(scenario, func(test *testing.T) {
			fixture, scope, storage := newMinimumOwnerFixture(test, 1)
			minimum := newMinimumOwnerQueries(test, scope, storage)
			requester := &preparedOwnerRequester{test: test, scope: scope}
			config := PreparedConfig{Scope: scope, Minimum: minimum, Repositories: []domain.ContentHash{fixture.target.RepoID}, Requester: requester}
			storage.readFailure = domain.ErrMemoryReadNotReady
			wantCalls := 0
			switch scenario {
			case "foreign-demand":
				config.DemandRepositories = []domain.ContentHash{domain.HashContent([]byte("foreign repository"))}
			case "configured-demand", "demand-denied":
				config.DemandRepositories, wantCalls = []domain.ContentHash{fixture.target.RepoID}, 1
			case "ready":
				config.DemandRepositories, storage.readFailure = []domain.ContentHash{fixture.target.RepoID}, nil
			}
			if scenario == "demand-denied" {
				requester.failure = domain.ErrForbidden
			}
			queries, err := NewPrepared(config)
			if err != nil {
				test.Fatal(err)
			}
			page, err := queries.QueryPreparedMemory(context.Background(), fixture.target.RepoID, fixture.request)
			var pending *domain.MemoryReadPending
			switch scenario {
			case "ready":
				if err != nil || len(page.Page.Items) != 1 {
					test.Fatal("ready page scheduled preparation", page, err)
				}
			case "demand-denied":
				if !errors.Is(err, domain.ErrForbidden) || errors.As(err, &pending) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
					test.Fatal("preparation's current authority error was hidden", page, err)
				}
			default:
				if !errors.As(err, &pending) || pending.Cursor == "" || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
					test.Fatal("pending lost the exact floor retry", page, err)
				}
			}
			if len(requester.calls) != wantCalls || scope.depth != 0 {
				test.Fatal("unconfigured demand executed or scope leaked", requester.calls, scope.depth)
			}
		})
	}
}

func TestPreparedOwnerKeepsFirstFloorThroughPendingGenerationAndReplay(test *testing.T) {
	fixture, scope, storage := newMinimumOwnerFixture(test, 2)
	minimum := newMinimumOwnerQueries(test, scope, storage)
	requester := &preparedOwnerRequester{test: test, scope: scope}
	queries := newPreparedOwnerQueries(test, fixture, scope, minimum, requester, true)
	storage.readFailure = domain.ErrMemoryReadNotReady
	request := fixture.request
	request.Limit = 1
	retry := encodeMinimumOwnerJSON(test, PreparedCursor{Version: 1, Requirement: fixture.requirement})
	for range 3 {
		page, err := queries.QueryPreparedMemory(context.Background(), fixture.target.RepoID, request)
		var pending *domain.MemoryReadPending
		if !errors.As(err, &pending) || !errors.Is(err, domain.ErrMemoryReadNotReady) || pending.Cursor != retry || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
			test.Fatal("pending changed the fixed-floor cursor or returned partial data", page, err)
		}
		request.Cursor = pending.Cursor
		storage.requirement.Minimum.Sequence++
	}
	storage.readFailure = nil
	storage.status.Observed.Sequence++
	storage.status.Freshness = "catching_up"
	page, err := queries.QueryPreparedMemory(context.Background(), fixture.target.RepoID, request)
	replay := encodeMinimumOwnerJSON(test, PreparedCursor{Version: 1, Requirement: fixture.requirement, Generation: fixture.bundle.ID})
	next := encodeMinimumOwnerJSON(test, PreparedCursor{Version: 1, Requirement: fixture.requirement, Generation: fixture.bundle.ID, Continuation: minimumOwnerContinuation(test, fixture, 1)})
	if err != nil || page.Cursor != replay || page.Page.NextCursor != next || page.Status != storage.status || !reflect.DeepEqual(page.Page.Items, fixture.items[:1]) {
		test.Fatal("first ready result lost original floor, generation, or exact cursors", page, err)
	}
	if len(next) <= 1024 || len(next) > domain.MaxPreparedMemoryCursorBytes {
		test.Fatal("fixture did not exercise the outer prepared cursor bound", len(next))
	}
	request.Cursor = page.Page.NextCursor
	last, err := queries.QueryPreparedMemory(context.Background(), fixture.target.RepoID, request)
	if err != nil || last.Cursor != next || last.Page.NextCursor != "" || !reflect.DeepEqual(last.Page.Items, fixture.items[1:]) {
		test.Fatal("continuation changed generation or replay position", last, err)
	}
	request.Cursor = last.Cursor
	replayed, err := queries.QueryPreparedMemory(context.Background(), fixture.target.RepoID, request)
	if err != nil || !reflect.DeepEqual(replayed, last) {
		test.Fatal("returned cursor did not replay the exact page", replayed, err)
	}
	if len(storage.observed) != 1 || len(storage.reads) != 6 || len(requester.calls) != 3 || scope.admissions != 6 || scope.snapshotCalls != 7 || scope.depth != 0 {
		test.Fatal("retry/replay reobserved or introduced another scope owner", len(storage.observed), storage.reads, requester.calls, scope)
	}
	for ordinal, read := range storage.reads {
		generation := domain.ContentHash("")
		if ordinal >= 4 {
			generation = fixture.bundle.ID
		}
		if read != (minimumOwnerRead{target: fixture.target, requirement: fixture.requirement, generation: generation}) {
			test.Fatal("read advanced the original floor or fixed generation", ordinal, read)
		}
	}
	for _, demand := range requester.calls {
		if demand != (minimumOwnerRead{target: fixture.target, requirement: fixture.requirement}) {
			test.Fatal("demand advanced the first observed floor", demand)
		}
	}
}

func TestPreparedOwnerOnlyPurePendingBecomesRetryOrStale(test *testing.T) {
	fixture, scope, storage := newMinimumOwnerFixture(test, 1)
	minimum := newMinimumOwnerQueries(test, scope, storage)
	requester := &preparedOwnerRequester{test: test, scope: scope}
	queries := newPreparedOwnerQueries(test, fixture, scope, minimum, requester, true)
	pinned := encodeMinimumOwnerJSON(test, PreparedCursor{Version: 1, Requirement: fixture.requirement, Generation: fixture.bundle.ID})
	for _, failure := range []error{
		domain.ErrMemoryReadNotReady,
		fmt.Errorf("wrapped pending: %w", domain.ErrMemoryReadNotReady),
		errors.Join(domain.ErrMemoryReadNotReady, domain.ErrMemoryReadNotReady),
	} {
		for _, encoded := range []string{"", pinned} {
			storage.readFailure = failure
			request := fixture.request
			request.Cursor = encoded
			before := len(requester.calls)
			page, err := queries.QueryPreparedMemory(context.Background(), fixture.target.RepoID, request)
			var pending *domain.MemoryReadPending
			if encoded == "" {
				if !errors.As(err, &pending) || pending.Cursor == "" || len(requester.calls) != before+1 {
					test.Fatal("pure pending lost retry/demand", err)
				}
			} else if !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) || errors.As(err, &pending) || len(requester.calls) != before {
				test.Fatal("fixed generation pending was not stale or scheduled new work", err)
			}
			if !reflect.DeepEqual(page, domain.MemoryReadPage{}) || scope.depth != 0 {
				test.Fatal("pending/stale returned a partial page or retained scope", page)
			}
		}
	}
	for _, cause := range []error{
		domain.ErrForbidden, domain.ErrUnauthorized, domain.ErrIntegrity, domain.ErrCatalogUnsupported,
		domain.ErrMemoryReadNeedsAttention, domain.ErrMemoryCaptureExpired, domain.ErrCatalogResetRequired,
		domain.ErrValidation, domain.ErrEffectiveMemoryCursorStale, context.Canceled, errors.New("unexpected store failure"),
	} {
		for _, encoded := range []string{"", pinned} {
			for _, failure := range []error{cause, errors.Join(cause, domain.ErrMemoryReadNotReady), errors.Join(domain.ErrMemoryReadNotReady, cause)} {
				storage.readFailure = fmt.Errorf("read failure: %w", failure)
				request := fixture.request
				request.Cursor = encoded
				before := len(requester.calls)
				page, err := queries.QueryPreparedMemory(context.Background(), fixture.target.RepoID, request)
				var pending *domain.MemoryReadPending
				if !errors.Is(err, cause) || errors.As(err, &pending) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) || len(requester.calls) != before || scope.depth != 0 {
					test.Fatal("non-pending cause was discarded or triggered preparation", cause, page, err)
				}
			}
		}
	}
}

type preparedOwnerPendingLookalike struct{}

func (preparedOwnerPendingLookalike) Error() string { return "not a pure pending cause" }

func (preparedOwnerPendingLookalike) Is(target error) bool {
	return target == domain.ErrMemoryReadNotReady
}

func TestPreparedOwnerDoesNotClassifyPendingByErrorsIsAlone(test *testing.T) {
	fixture, scope, storage := newMinimumOwnerFixture(test, 1)
	minimum := newMinimumOwnerQueries(test, scope, storage)
	requester := &preparedOwnerRequester{test: test, scope: scope}
	queries := newPreparedOwnerQueries(test, fixture, scope, minimum, requester, true)
	failure := preparedOwnerPendingLookalike{}
	storage.readFailure = failure
	page, err := queries.QueryPreparedMemory(context.Background(), fixture.target.RepoID, fixture.request)
	var pending *domain.MemoryReadPending
	if err != failure || !errors.Is(err, domain.ErrMemoryReadNotReady) || errors.As(err, &pending) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) || len(requester.calls) != 0 {
		test.Fatal("errors.Is-only match was turned into pending", page, err)
	}
}

func TestPreparedOwnerReauthorizesCurrentSnapshotOnEveryRequest(test *testing.T) {
	for _, scenario := range []string{"observe", "pending-retry", "generation-replay", "generation-continuation"} {
		test.Run(scenario, func(test *testing.T) {
			fixture, scope, storage := newMinimumOwnerFixture(test, 2)
			minimum := newMinimumOwnerQueries(test, scope, storage)
			requester := &preparedOwnerRequester{test: test, scope: scope}
			queries := newPreparedOwnerQueries(test, fixture, scope, minimum, requester, true)
			request := fixture.request
			request.Limit = 1
			if scenario == "pending-retry" {
				storage.readFailure = domain.ErrMemoryReadNotReady
				_, err := queries.QueryPreparedMemory(context.Background(), fixture.target.RepoID, request)
				var pending *domain.MemoryReadPending
				if !errors.As(err, &pending) {
					test.Fatal(err)
				}
				request.Cursor = pending.Cursor
			} else if scenario != "observe" {
				page, err := queries.QueryPreparedMemory(context.Background(), fixture.target.RepoID, request)
				if err != nil {
					test.Fatal(err)
				}
				request.Cursor = page.Cursor
				if scenario == "generation-continuation" {
					request.Cursor = page.Page.NextCursor
				}
			}
			beforeObserved, beforeDemand, beforeSnapshot := len(storage.observed), len(requester.calls), scope.snapshotCalls
			storage.beforeAuthorize = func() { scope.snapshotFailure = domain.ErrForbidden }
			page, err := queries.QueryPreparedMemory(context.Background(), fixture.target.RepoID, request)
			var pending *domain.MemoryReadPending
			if !errors.Is(err, domain.ErrForbidden) || errors.As(err, &pending) || errors.Is(err, domain.ErrEffectiveMemoryCursorStale) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) || len(requester.calls) != beforeDemand || scope.snapshotCalls != beforeSnapshot+1 || scope.depth != 0 {
				test.Fatal("old observation/generation bypassed current authority or masked revocation", page, err)
			}
			if scenario != "observe" && len(storage.observed) != beforeObserved {
				test.Fatal("reauthorization reobserved the pinned floor")
			}
		})
	}
}

func TestPreparedOwnerBorrowsOnlyInjectedActiveScope(test *testing.T) {
	fixture, scope, storage := newMinimumOwnerFixture(test, 1)
	minimum := newMinimumOwnerQueries(test, scope, storage)
	queries := newPreparedOwnerQueries(test, fixture, scope, minimum, nil, false)
	var escaped context.Context
	err := scope.WithMemoryReadResources(context.Background(), fixture.target.RepoID, func(bound context.Context) error {
		escaped = bound
		page, err := queries.QueryPreparedMemory(bound, fixture.target.RepoID, fixture.request)
		if err != nil || len(page.Page.Items) != 1 || scope.admissions != 1 || scope.snapshotCalls != 2 {
			test.Fatal("prepared/minimum added another admission owner", page, err, scope)
		}
		return err
	})
	if err != nil || scope.depth != 0 {
		test.Fatal("caller scope did not release", err)
	}
	if _, err := queries.QueryPreparedMemory(escaped, fixture.target.RepoID, fixture.request); err != nil || scope.admissions != 2 || scope.snapshotCalls != 4 || scope.depth != 0 {
		test.Fatal("expired caller scope was reused", err, scope)
	}
}

func TestPreparedOwnerCursorKeepsWireAndCanonicalBounds(test *testing.T) {
	fixture, _, _ := newMinimumOwnerFixture(test, 1)
	original := PreparedCursor{Version: 1, Requirement: fixture.requirement, Generation: fixture.bundle.ID}
	for _, continuation := range []string{"", "inner-cursor", strings.Repeat("c", 1024)} {
		cursor := original
		cursor.Continuation = continuation
		encoded, err := EncodePreparedCursor(cursor)
		if err != nil {
			test.Fatal(err)
		}
		requirement, err := json.Marshal(cursor.Requirement)
		if err != nil {
			test.Fatal(err)
		}
		wire := fmt.Sprintf(`{"version":1,"requirement":%s,"generation":%q,"continuation":%q}`, requirement, cursor.Generation, continuation)
		if encoded != base64.RawURLEncoding.EncodeToString([]byte(wire)) {
			test.Fatal("exported cursor changed original fields/tags or canonical wire bytes")
		}
		if decoded, err := DecodePreparedCursor(encoded, fixture.target); err != nil || decoded != cursor {
			test.Fatal("cursor roundtrip lost floor/generation/position", decoded, err)
		}
	}
	pending := original
	pending.Generation = ""
	if decoded, err := DecodePreparedCursor(encodeMinimumOwnerJSON(test, pending), fixture.target); err != nil || decoded != pending {
		test.Fatal("pending cursor lost the fixed minimum", decoded, err)
	}
	for _, scenario := range []string{"version", "missing-generation", "bad-generation", "inner-limit", "wrong-target", "wrong-epoch", "unknown-field", "duplicate-field", "whitespace", "reordered", "oversize", "base64", "padding", "null", "legacy", "noncanonical-base64"} {
		test.Run(scenario, func(test *testing.T) {
			cursor := original
			switch scenario {
			case "version":
				cursor.Version++
			case "missing-generation":
				cursor.Generation, cursor.Continuation = "", "page"
			case "bad-generation":
				cursor.Generation = "invalid"
			case "inner-limit":
				cursor.Continuation = strings.Repeat("c", 1025)
			case "wrong-target":
				cursor.Requirement.TargetID = domain.HashContent([]byte("foreign target"))
			case "wrong-epoch":
				cursor.Requirement.Minimum.Epoch = "invalid"
			}
			raw, err := json.Marshal(cursor)
			if err != nil {
				test.Fatal(err)
			}
			switch scenario {
			case "unknown-field":
				raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"unknown":true}`)
			case "duplicate-field":
				raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"version":1}`)
			case "whitespace":
				raw = append(raw, ' ')
			case "reordered":
				raw = []byte(strings.Replace(string(raw), `{"version":1,`, `{`, 1))
				raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"version":1}`)
			case "null":
				raw = []byte("null")
			case "legacy":
				raw = []byte(`{"v":1,"index":1}`)
			case "noncanonical-base64":
				for len(raw)%3 == 0 {
					cursor.Continuation += "c"
					raw, err = json.Marshal(cursor)
					if err != nil {
						test.Fatal(err)
					}
				}
			}
			encoded := base64.RawURLEncoding.EncodeToString(raw)
			switch scenario {
			case "oversize":
				encoded = strings.Repeat("a", domain.MaxPreparedMemoryCursorBytes+1)
			case "base64":
				encoded = "%"
			case "padding":
				encoded += "="
			case "noncanonical-base64":
				alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
				last := strings.IndexByte(alphabet, encoded[len(encoded)-1])
				encoded = encoded[:len(encoded)-1] + string(alphabet[last+1])
			}
			if decoded, err := DecodePreparedCursor(encoded, fixture.target); err == nil || decoded != (PreparedCursor{}) {
				test.Fatal("malformed/noncanonical cursor accepted", decoded, err)
			}
		})
	}
	tooLarge := original
	tooLarge.Continuation = strings.Repeat("c", domain.MaxPreparedMemoryCursorBytes)
	if encoded, err := EncodePreparedCursor(tooLarge); !errors.Is(err, domain.ErrMemoryProjectionLimit) || encoded != "" {
		test.Fatal("oversized output cursor accepted", encoded, err)
	}
}

func TestPreparedOwnerValidatesNestedContinuationWithoutReobservation(test *testing.T) {
	for _, scenario := range []string{"wrong-floor", "wrong-generation", "bad-minimum", "stale-page", "past-end", "changed-selection"} {
		test.Run(scenario, func(test *testing.T) {
			fixture, scope, storage := newMinimumOwnerFixture(test, 2)
			minimum := newMinimumOwnerQueries(test, scope, storage)
			queries := newPreparedOwnerQueries(test, fixture, scope, minimum, nil, false)
			cursor := PreparedCursor{Version: 1, Requirement: fixture.requirement, Generation: fixture.bundle.ID, Continuation: minimumOwnerContinuation(test, fixture, 1)}
			request := fixture.request
			want := domain.ErrEffectiveMemoryCursorStale
			switch scenario {
			case "wrong-floor":
				cursor.Requirement.Minimum.Sequence++
			case "wrong-generation":
				cursor.Generation = domain.HashContent([]byte("new generation"))
			case "bad-minimum":
				cursor.Continuation, want = "!", domain.ErrValidation
			case "stale-page", "past-end":
				inner := Cursor{Version: 1, State: domain.HashContent([]byte("old page state")), Index: 1}
				if scenario == "past-end" {
					inner.State, inner.Index, want = fixture.bundle.Header.View.StateHash, 3, domain.ErrValidation
				}
				raw, err := json.Marshal(fixture.requirement)
				if err != nil {
					test.Fatal(err)
				}
				cursor.Continuation = encodeMinimumOwnerJSON(test, minimumMemoryCursor{
					Version: 1, Requirement: domain.HashContent(raw), Generation: fixture.bundle.ID,
					Continuation: encodeMinimumOwnerJSON(test, inner),
				})
			case "changed-selection":
				request.Selection.CodeCommit, want = strings.Repeat("b", 40), domain.ErrValidation
			}
			request.Cursor = encodeMinimumOwnerJSON(test, cursor)
			page, err := queries.QueryPreparedMemory(context.Background(), fixture.target.RepoID, request)
			if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) || len(storage.observed) != 0 || scope.depth != 0 {
				test.Fatal("invalid nested continuation returned data or observed a new floor", page, err)
			}
		})
	}
}

func TestPreparedOwnerRequestValidationAndCancellation(test *testing.T) {
	for _, scenario := range []string{"invalid-selection", "invalid-content", "limit", "outer-limit", "malformed-cursor", "canceled", "canceled-chunk", "scope-close", "observation-failure"} {
		test.Run(scenario, func(test *testing.T) {
			fixture, scope, storage := newMinimumOwnerFixture(test, 2)
			minimum := newMinimumOwnerQueries(test, scope, storage)
			queries := newPreparedOwnerQueries(test, fixture, scope, minimum, nil, false)
			request := fixture.request
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := domain.ErrValidation
			switch scenario {
			case "invalid-selection":
				request.Selection.CodeCommit = "invalid"
			case "invalid-content":
				request.Content = "unsupported"
			case "limit":
				request.Limit = -1
			case "outer-limit":
				request.Cursor = strings.Repeat("a", domain.MaxPreparedMemoryCursorBytes+1)
			case "malformed-cursor":
				request.Cursor = "!"
			case "canceled":
				cancel()
				want = context.Canceled
			case "canceled-chunk":
				storage.cancelChunk, want = cancel, context.Canceled
			case "scope-close":
				scope.exitFailure, want = domain.ErrIntegrity, domain.ErrIntegrity
			case "observation-failure":
				storage.observeFailure, want = domain.ErrCatalogResetRequired, domain.ErrCatalogResetRequired
			}
			page, err := queries.QueryPreparedMemory(ctx, fixture.target.RepoID, request)
			if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) || scope.depth != 0 {
				test.Fatal("invalid/canceled read returned partial data or leaked scope", page, err)
			}
			switch scenario {
			case "invalid-selection", "invalid-content", "limit", "outer-limit", "malformed-cursor", "canceled":
				if len(storage.observed) != 0 || len(storage.reads) != 0 {
					test.Fatal("invalid/canceled request accessed the explicit read port")
				}
			}
		})
	}
}
