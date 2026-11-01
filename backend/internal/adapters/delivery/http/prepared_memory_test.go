package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type preparedMemoryQueryFunc func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error)

func (query preparedMemoryQueryFunc) QueryPreparedMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
	return query(ctx, repo, request)
}

type scopedPreparedMemoryQuery struct {
	inbound.MemoryReadScope
	preparedMemoryQueryFunc
}

type preparedMemoryScopeFunc func(context.Context, domain.ContentHash, func(context.Context) error) error

func (scope preparedMemoryScopeFunc) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope(ctx, repo, read)
}

func requirePreparedMemoryReader(test *testing.T, server *Server, reader inbound.PreparedMemoryReader) {
	test.Helper()
	if err := server.SetPreparedMemory(reader); err != nil {
		test.Fatal(err)
	}
}

func preparedMemoryHTTPRequest(fixture memoryAdmissionHTTPFixture, values url.Values) *http.Request {
	request := fixture.request(context.Background(), "effective")
	request.URL.Path += "/read"
	if values != nil {
		request.URL.RawQuery = values.Encode()
	}
	return request
}

func preparedMemoryHTTPPage(fixture memoryAdmissionHTTPFixture) domain.MemoryReadPage {
	source := domain.RepositoryMemorySourceGeneration{
		Version: domain.RepositoryMemorySourceVersion,
		Graph: domain.RepositoryViewSourceCheckpoint{
			Version:  domain.RepositoryViewSourceVersion,
			Scope:    domain.RepositoryGraphSourceScope,
			RepoID:   fixture.repo,
			Epoch:    "11111111-1111-4111-8111-111111111111",
			Sequence: 1<<53 + 1,
		},
		Epoch: "22222222-2222-4222-8222-222222222222", Sequence: 1<<53 + 3,
	}
	return domain.MemoryReadPage{
		Cursor: "prepared-replay",
		Status: domain.MemoryReadStatus{
			Version: domain.MemoryReadVersion, Rules: domain.MemoryReadRules,
			Generation: domain.HashContent([]byte("prepared generation")),
			Minimum:    source, Observed: source, Applied: source, Complete: true, Freshness: "current",
		},
		Page: domain.EffectiveMemoryPage{
			Selection: domain.EffectiveMemorySelection{SnapshotID: fixture.snapshot, CodeCommit: strings.Repeat("1", 40), MemoryHash: fixture.memory},
			StateHash: domain.HashContent([]byte("prepared state")), LineageHash: domain.HashContent([]byte("prepared lineage")),
			Items: []domain.EffectiveMemoryItem{{ID: fixture.memory, SourceSnapshot: fixture.snapshot, Kind: "rationale", Text: "Private prepared memory",
				MemoryClaimAssessment: domain.MemoryClaimAssessment{State: "retained", Reason: "historical_knowledge"}}},
			Total: 2, NextCursor: strings.Repeat("n", domain.MaxPreparedMemoryCursorBytes),
		},
	}
}

func TestPreparedMemoryHTTPReady(test *testing.T) {
	for _, mode := range []string{"defaults", "branch", "continuation"} {
		test.Run(mode, func(test *testing.T) {
			fixture := newMemoryAdmissionHTTPFixture(test)
			page := preparedMemoryHTTPPage(fixture)
			request := preparedMemoryHTTPRequest(fixture, nil)
			expected := domain.EffectiveMemoryRequest{Selection: page.Page.Selection}
			values := request.URL.Query()
			switch mode {
			case "branch":
				expected.Selection.Branch, expected.Selection.MemoryHash = "main", ""
				expected.Content, expected.Limit = "claims", 50
				values.Set("branch", "main")
				values.Del("memory_hash")
				values.Set("content", "claims")
				values.Set("limit", "50")
				page.Status.Observed.Sequence++
				page.Status.Freshness = "catching_up"
			case "continuation":
				expected.Content, expected.Limit, expected.Cursor = "prompt", 1, strings.Repeat("c", domain.MaxPreparedMemoryCursorBytes)
				values.Set("content", "prompt")
				values.Set("limit", "1")
				values.Set("cursor", expected.Cursor)
			}
			request.URL.RawQuery = values.Encode()
			page.Page.Selection, page.Page.Content = expected.Selection, expected.Content
			if err := page.Status.Validate(); err != nil {
				test.Fatal(err)
			}
			calls := 0
			requirePreparedMemoryReader(test, fixture.server, scopedPreparedMemoryQuery{fixture.backend, func(ctx context.Context, repo domain.ContentHash, input domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
				calls++
				actor, _ := inbound.RepositoryActor(ctx)
				if _, ok := ctx.Deadline(); !ok || actor != fixture.viewer.ID || repo != fixture.repo || input != expected {
					test.Fatalf("unbound prepared request: actor=%q repo=%s input=%+v", actor, repo, input)
				}
				return page, nil
			}})
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, request)
			var actual domain.MemoryReadPage
			if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil {
				test.Fatal(err)
			}
			if response.Code != http.StatusOK || !reflect.DeepEqual(actual, page) || calls != 1 || fixture.backend.scopes.Load() != 1 || fixture.backend.authorizations.Load() != 1 {
				test.Fatalf("prepared response: %d %s calls=%d scopes=%d authorizations=%d", response.Code, response.Body, calls, fixture.backend.scopes.Load(), fixture.backend.authorizations.Load())
			}
			if !strings.Contains(response.Body.String(), `"sequence":"9007199254740993"`) || response.Header().Get("Retry-After") != "" || fixture.backend.reads.Load() != 0 {
				test.Fatalf("prepared wire contract or legacy fallback: %s", response.Body)
			}
			memoryAdmissionAssertReleased(test)
		})
	}
}

func TestPreparedMemoryHTTPDeniedBeforeRead(test *testing.T) {
	for _, token := range []string{"", "dev:outsider@example.test:Other"} {
		test.Run(token, func(test *testing.T) {
			fixture := newMemoryAdmissionHTTPFixture(test)
			requirePreparedMemoryReader(test, fixture.server, scopedPreparedMemoryQuery{fixture.backend, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
				test.Fatal("denied request reached prepared query")
				return domain.MemoryReadPage{}, nil
			}})
			request := preparedMemoryHTTPRequest(fixture, nil)
			request.Header.Del("Authorization")
			wantStatus, wantCode := http.StatusUnauthorized, "unauthenticated"
			if token != "" {
				request.Header.Set("Authorization", "Bearer "+token)
				wantStatus, wantCode = http.StatusForbidden, "forbidden"
			}
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, request)
			assertPreparedMemoryHTTPError(test, response, wantStatus, wantCode, "", "")
			if fixture.backend.scopes.Load() != 0 || fixture.backend.reads.Load() != 0 {
				test.Fatal("denied request reached admission or legacy reads")
			}
		})
	}
}

func TestPreparedMemoryHTTPInvalidParameters(test *testing.T) {
	fixture := newMemoryAdmissionHTTPFixture(test)
	requirePreparedMemoryReader(test, fixture.server, scopedPreparedMemoryQuery{fixture.backend, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
		test.Fatal("invalid request reached prepared query")
		return domain.MemoryReadPage{}, nil
	}})
	for _, invalid := range []struct{ key, value string }{
		{"limit", ""}, {"limit", "zero"}, {"limit", "0"}, {"limit", "-1"}, {"limit", "51"}, {"limit", strings.Repeat("9", 40)},
		{"snapshot_id", ""}, {"snapshot_id", "invalid"}, {"code_commit", ""}, {"code_commit", "main"},
		{"memory_hash", "invalid"}, {"branch", "main"}, {"branch", "../bad"}, {"content", "unknown"},
		{"cursor", strings.Repeat("c", domain.MaxPreparedMemoryCursorBytes+1)},
	} {
		test.Run(invalid.key+"/"+invalid.value[:min(len(invalid.value), 16)], func(test *testing.T) {
			request := preparedMemoryHTTPRequest(fixture, nil)
			values := request.URL.Query()
			values.Set(invalid.key, invalid.value)
			request.URL.RawQuery = values.Encode()
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, request)
			assertPreparedMemoryHTTPError(test, response, http.StatusUnprocessableEntity, "validation", "", "")
		})
	}
	if fixture.backend.scopes.Load() != 0 || fixture.backend.reads.Load() != 0 {
		test.Fatal("invalid request reached admission or legacy reads")
	}
}

func TestPreparedMemoryHTTPRejectsMalformedQuery(test *testing.T) {
	fixture := newMemoryAdmissionHTTPFixture(test)
	requirePreparedMemoryReader(test, fixture.server, scopedPreparedMemoryQuery{fixture.backend, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
		test.Fatal("malformed query reached prepared read")
		return domain.MemoryReadPage{}, nil
	}})
	for _, suffix := range []string{
		"cursor=%ZZ", "cursor=%", "cursor=%2", "cursor=", "cursor",
		"unrelated=%ZZ", "unrelated=%", "%ZZ=value", "unrelated=one;two",
	} {
		test.Run(suffix, func(test *testing.T) {
			request := preparedMemoryHTTPRequest(fixture, nil)
			request.URL.RawQuery += "&" + suffix
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, request)
			assertPreparedMemoryHTTPError(test, response, http.StatusUnprocessableEntity, "validation", "", "")
		})
	}
	if fixture.backend.scopes.Load() != 0 || fixture.backend.authorizations.Load() != 0 || fixture.backend.reads.Load() != 0 {
		test.Fatal("malformed query reached admission or legacy reads")
	}
}

func TestPreparedMemoryHTTPRejectsDuplicateParameters(test *testing.T) {
	fixture := newMemoryAdmissionHTTPFixture(test)
	requirePreparedMemoryReader(test, fixture.server, scopedPreparedMemoryQuery{fixture.backend, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
		test.Fatal("ambiguous query reached prepared read")
		return domain.MemoryReadPage{}, nil
	}})
	for _, duplicate := range []struct{ name, first, second string }{
		{"branch", "", "main"},
		{"snapshot_id", string(fixture.snapshot), string(domain.HashContent([]byte("another snapshot")))},
		{"code_commit", strings.Repeat("1", 40), strings.Repeat("2", 40)},
		{"memory_hash", string(fixture.memory), string(domain.HashContent([]byte("another memory")))},
		{"content", "all", "claims"},
		{"cursor", "pinned-cursor", "another-cursor"},
		{"limit", "1", "2"},
	} {
		for _, identical := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/identical=%t", duplicate.name, identical), func(test *testing.T) {
				request := preparedMemoryHTTPRequest(fixture, nil)
				values := request.URL.Query()
				values.Set(duplicate.name, duplicate.first)
				second := duplicate.second
				if identical {
					second = duplicate.first
				}
				values.Add(duplicate.name, second)
				request.URL.RawQuery = values.Encode()
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, request)
				assertPreparedMemoryHTTPError(test, response, http.StatusUnprocessableEntity, "validation", "", "")
			})
		}
	}
	test.Run("encoded-duplicate-key", func(test *testing.T) {
		request := preparedMemoryHTTPRequest(fixture, nil)
		request.URL.RawQuery += "&cursor=pinned-cursor&%63ursor=another-cursor"
		response := httptest.NewRecorder()
		fixture.server.Handler().ServeHTTP(response, request)
		assertPreparedMemoryHTTPError(test, response, http.StatusUnprocessableEntity, "validation", "", "")
	})
	if fixture.backend.scopes.Load() != 0 || fixture.backend.authorizations.Load() != 0 || fixture.backend.reads.Load() != 0 {
		test.Fatal("ambiguous query reached admission or legacy reads")
	}
}

func TestPreparedMemoryHTTPErrorContract(test *testing.T) {
	fixture := newMemoryAdmissionHTTPFixture(test)
	for _, failure := range []struct {
		name, code, retryAfter, retryCursor string
		err                                 error
		status                              int
	}{
		{name: "pending", code: "memory_read_not_ready", retryAfter: "2", err: domain.ErrMemoryReadNotReady, status: http.StatusServiceUnavailable},
		{name: "pending_cursor", code: "memory_read_not_ready", retryAfter: "2", retryCursor: "pinned-request-floor", err: &domain.MemoryReadPending{Cursor: "pinned-request-floor"}, status: http.StatusServiceUnavailable},
		{name: "pending_cursor_too_long", code: "memory_read_not_ready", retryAfter: "2", err: &domain.MemoryReadPending{Cursor: strings.Repeat("x", domain.MaxPreparedMemoryCursorBytes+1)}, status: http.StatusServiceUnavailable},
		{name: "pending_only_nested", code: "memory_read_not_ready", retryAfter: "2", retryCursor: "pinned-request-floor", err: errors.Join(domain.ErrMemoryReadNotReady, errors.Join(&domain.MemoryReadPending{Cursor: "pinned-request-floor"}, domain.ErrMemoryReadNotReady)), status: http.StatusServiceUnavailable},
		{name: "pending_only_wrapped", code: "memory_read_not_ready", retryAfter: "2", retryCursor: "pinned-request-floor", err: fmt.Errorf("private pending wrapper: %w", errors.Join(&domain.MemoryReadPending{Cursor: "pinned-request-floor"}, domain.ErrMemoryReadNotReady)), status: http.StatusServiceUnavailable},
		{name: "pending_unknown", code: "internal", err: errors.Join(&domain.MemoryReadPending{Cursor: "must-not-leak"}, errors.New("private unknown error")), status: http.StatusInternalServerError},
		{name: "unknown_pending", code: "internal", err: errors.Join(errors.New("private unknown error"), &domain.MemoryReadPending{Cursor: "must-not-leak"}), status: http.StatusInternalServerError},
		{name: "pending_unknown_nested", code: "internal", err: errors.Join(&domain.MemoryReadPending{Cursor: "must-not-leak"}, errors.Join(domain.ErrMemoryReadNotReady, errors.New("private nested error"))), status: http.StatusInternalServerError},
		{name: "pending_unknown_wrapped", code: "internal", err: fmt.Errorf("private mixed wrapper: %w", errors.Join(fmt.Errorf("private pending wrapper: %w", &domain.MemoryReadPending{Cursor: "must-not-leak"}), fmt.Errorf("private unknown wrapper: %w", errors.New("private unknown error")))), status: http.StatusInternalServerError},
		{name: "attention", code: "memory_read_needs_attention", err: domain.ErrMemoryReadNeedsAttention, status: http.StatusServiceUnavailable},
		{name: "expired", code: "memory_capture_expired", err: domain.ErrMemoryCaptureExpired, status: http.StatusGone},
		{name: "reset", code: "catalog_reset_required", err: domain.ErrCatalogResetRequired, status: http.StatusConflict},
		{name: "stale", code: "memory_cursor_stale", err: domain.ErrEffectiveMemoryCursorStale, status: http.StatusConflict},
		{name: "unsupported", code: "prepared_memory_unavailable", err: domain.ErrCatalogUnsupported, status: http.StatusServiceUnavailable},
		{name: "overloaded", code: "resource_overloaded", retryAfter: "1", err: domain.ErrResourceOverloaded, status: http.StatusServiceUnavailable},
		{name: "validation", code: "validation", err: domain.ErrValidation, status: http.StatusUnprocessableEntity},
		{name: "integrity", code: "integrity_violation", err: domain.ErrIntegrity, status: http.StatusUnprocessableEntity},
		{name: "limit", code: "memory_projection_limit", err: domain.ErrMemoryProjectionLimit, status: http.StatusUnprocessableEntity},
		{name: "internal", code: "internal", err: errors.New("private internal error"), status: http.StatusInternalServerError},
		{name: "unauthorized_pending", code: "unauthenticated", err: errors.Join(domain.ErrUnauthorized, &domain.MemoryReadPending{Cursor: "must-not-leak"}), status: http.StatusUnauthorized},
		{name: "forbidden_pending", code: "forbidden", err: errors.Join(domain.ErrForbidden, &domain.MemoryReadPending{Cursor: "must-not-leak"}), status: http.StatusForbidden},
		{name: "integrity_pending", code: "integrity_violation", err: errors.Join(domain.ErrIntegrity, &domain.MemoryReadPending{Cursor: "must-not-leak"}), status: http.StatusUnprocessableEntity},
		{name: "attention_pending", code: "memory_read_needs_attention", err: errors.Join(domain.ErrMemoryReadNeedsAttention, &domain.MemoryReadPending{Cursor: "must-not-leak"}), status: http.StatusServiceUnavailable},
		{name: "validation_pending", code: "validation", err: errors.Join(domain.ErrValidation, &domain.MemoryReadPending{Cursor: "must-not-leak"}), status: http.StatusUnprocessableEntity},
		{name: "not_found_pending", code: "not_found", err: errors.Join(domain.ErrNotFound, &domain.MemoryReadPending{Cursor: "must-not-leak"}), status: http.StatusNotFound},
		{name: "limit_pending", code: "memory_projection_limit", err: errors.Join(domain.ErrMemoryProjectionLimit, &domain.MemoryReadPending{Cursor: "must-not-leak"}), status: http.StatusUnprocessableEntity},
	} {
		for _, phase := range []string{"query", "admission"} {
			test.Run(failure.name+"/"+phase, func(test *testing.T) {
				wrapped := fmt.Errorf("private wrapped payload: %w", failure.err)
				var scope inbound.MemoryReadScope = fixture.backend
				if phase == "admission" {
					scope = preparedMemoryScopeFunc(func(context.Context, domain.ContentHash, func(context.Context) error) error { return wrapped })
				}
				requirePreparedMemoryReader(test, fixture.server, scopedPreparedMemoryQuery{scope, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
					if phase == "admission" {
						test.Fatal("failed admission reached prepared query")
					}
					return preparedMemoryHTTPPage(fixture), wrapped
				}})
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, preparedMemoryHTTPRequest(fixture, nil))
				assertPreparedMemoryHTTPError(test, response, failure.status, failure.code, failure.retryAfter, failure.retryCursor)
				memoryAdmissionAssertReleased(test)
			})
		}
	}
}

func assertPreparedMemoryHTTPError(test *testing.T, response *httptest.ResponseRecorder, status int, code, retryAfter, retryCursor string) {
	test.Helper()
	var envelope struct {
		Error struct {
			Code    string            `json:"code"`
			Message string            `json:"message"`
			Details map[string]string `json:"details"`
		} `json:"error"`
	}
	decoder := json.NewDecoder(strings.NewReader(response.Body.String()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		test.Fatal(err)
	}
	if response.Code != status || envelope.Error.Code != code || response.Header().Get("Retry-After") != retryAfter || envelope.Error.Details["retry_cursor"] != retryCursor || envelope.Error.Message == "" {
		test.Fatalf("error contract: status=%d retry=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body)
	}
	if retryCursor == "" && len(envelope.Error.Details) != 0 || retryCursor != "" && len(envelope.Error.Details) != 1 {
		test.Fatalf("unexpected error details: %s", response.Body)
	}
	for _, secret := range []string{"private", "Private prepared memory", "prepared-replay", "must-not-leak"} {
		if strings.Contains(response.Body.String(), secret) {
			test.Fatalf("error leaked payload or internal details: %s", response.Body)
		}
	}
}

func TestPreparedMemoryHTTPPendingRetryPreservesCursor(test *testing.T) {
	fixture := newMemoryAdmissionHTTPFixture(test)
	retryCursor := strings.Repeat("r", domain.MaxPreparedMemoryCursorBytes)
	page := preparedMemoryHTTPPage(fixture)
	calls := 0
	requirePreparedMemoryReader(test, fixture.server, scopedPreparedMemoryQuery{fixture.backend, func(_ context.Context, _ domain.ContentHash, input domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
		calls++
		if calls == 1 {
			if input.Cursor != "" {
				test.Fatal("initial request unexpectedly carried a cursor")
			}
			return domain.MemoryReadPage{}, fmt.Errorf("private pending context: %w", &domain.MemoryReadPending{Cursor: retryCursor})
		}
		if input.Cursor != retryCursor {
			test.Fatal("retry lost its pinned request floor")
		}
		return page, nil
	}})
	response := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(response, preparedMemoryHTTPRequest(fixture, nil))
	assertPreparedMemoryHTTPError(test, response, http.StatusServiceUnavailable, "memory_read_not_ready", "2", retryCursor)
	var pending struct {
		Error struct {
			Details struct {
				RetryCursor string `json:"retry_cursor"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &pending); err != nil {
		test.Fatal(err)
	}
	request := preparedMemoryHTTPRequest(fixture, nil)
	values := request.URL.Query()
	values.Set("cursor", pending.Error.Details.RetryCursor)
	request.URL.RawQuery = values.Encode()
	response = httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(response, request)
	var actual domain.MemoryReadPage
	if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil {
		test.Fatal(err)
	}
	if response.Code != http.StatusOK || calls != 2 || !reflect.DeepEqual(actual, page) {
		test.Fatalf("retry response: %d %s calls=%d", response.Code, response.Body, calls)
	}
}

func TestPreparedMemoryHTTPUnavailableWithoutFallback(test *testing.T) {
	for _, mode := range []string{"disabled", "unavailable_owner", "unavailable_admission"} {
		test.Run(mode, func(test *testing.T) {
			fixture := newMemoryAdmissionHTTPFixture(test)
			if mode == "unavailable_owner" {
				reader, err := fixture.backend.Service.PreparedMemoryQueries()
				if err != nil {
					test.Fatal(err)
				}
				requirePreparedMemoryReader(test, fixture.server, reader)
			}
			if mode == "unavailable_admission" {
				scope := preparedMemoryScopeFunc(func(context.Context, domain.ContentHash, func(context.Context) error) error {
					return domain.ErrCatalogUnsupported
				})
				requirePreparedMemoryReader(test, fixture.server, scopedPreparedMemoryQuery{scope, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
					test.Fatal("prepared query bypassed admission")
					return domain.MemoryReadPage{}, nil
				}})
			}
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, preparedMemoryHTTPRequest(fixture, nil))
			assertPreparedMemoryHTTPError(test, response, http.StatusServiceUnavailable, "prepared_memory_unavailable", "", "")
			if fixture.backend.reads.Load() != 0 {
				test.Fatal("unavailable prepared read fell back to legacy query")
			}
			response = httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), "effective"))
			if response.Code != http.StatusOK || fixture.backend.reads.Load() != 1 || strings.Contains(response.Body.String(), `"status":`) {
				test.Fatalf("legacy route changed: %d %s", response.Code, response.Body)
			}
		})
	}
}

func TestPreparedMemoryHTTPLegacyErrorMappingUnchanged(test *testing.T) {
	server := &Server{}
	for _, failure := range []error{
		domain.ErrMemoryReadNotReady,
		&domain.MemoryReadPending{Cursor: "must-not-leak"},
		domain.ErrMemoryReadNeedsAttention,
		domain.ErrMemoryCaptureExpired,
		domain.ErrCatalogResetRequired,
		domain.ErrCatalogUnsupported,
	} {
		response := httptest.NewRecorder()
		server.respond(response, nil, fmt.Errorf("private wrapped payload: %w", failure))
		assertPreparedMemoryHTTPError(test, response, http.StatusInternalServerError, "internal", "", "")
	}
}

func TestPreparedMemoryHTTPSaturatedAdmission(test *testing.T) {
	fixture := newMemoryAdmissionHTTPFixture(test)
	requirePreparedMemoryReader(test, fixture.server, scopedPreparedMemoryQuery{fixture.backend, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
		test.Fatal("saturated admission reached prepared query")
		return domain.MemoryReadPage{}, nil
	}})
	_, release, err := app.AcquireResources(context.Background(), app.ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(release)
	response := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(response, preparedMemoryHTTPRequest(fixture, nil))
	assertPreparedMemoryHTTPError(test, response, http.StatusServiceUnavailable, "resource_overloaded", "1", "")
	if fixture.backend.scopes.Load() != 1 || fixture.backend.authorizations.Load() != 0 || !errors.Is(fixture.backend.scopeError, domain.ErrResourceOverloaded) {
		test.Fatalf("saturated admission bypass: scopes=%d authorizations=%d err=%v", fixture.backend.scopes.Load(), fixture.backend.authorizations.Load(), fixture.backend.scopeError)
	}
	release()
	memoryAdmissionAssertReleased(test)
}

func TestPreparedMemoryHTTPSchemaFields(test *testing.T) {
	for _, contract := range []struct {
		name  string
		value any
	}{
		{"MemoryReadPage", domain.MemoryReadPage{}},
		{"MemoryReadStatus", domain.MemoryReadStatus{}},
		{"MemoryReadSourceGeneration", domain.RepositoryMemorySourceGeneration{}},
	} {
		test.Run(contract.name, func(test *testing.T) {
			properties := schemaProps(test, "../../../../../schemas/openapi.yaml", contract.name)
			fields := jsonFields(reflect.TypeOf(contract.value))
			for _, field := range fields {
				if !properties[field] {
					test.Errorf("missing schema field %s", field)
				}
			}
			for property := range properties {
				if !slices.Contains(fields, property) {
					test.Errorf("phantom schema field %s", property)
				}
			}
		})
	}
}
