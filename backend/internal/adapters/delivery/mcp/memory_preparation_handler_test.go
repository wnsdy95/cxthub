package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/preparation"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/memorypreparation"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryPreparationDemandFunc func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error

func (prepare memoryPreparationDemandFunc) RequestPreparedMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) error {
	return prepare(ctx, repo, request)
}

type memoryPreparationPermitFunc func(context.Context, string, string) error

func (permit memoryPreparationPermitFunc) AuthorizeSnapshot(ctx context.Context, repository, user string) error {
	return permit(ctx, repository, user)
}

type memoryPreparationSnapshotKey struct{}

type memoryPreparationIdentity struct {
	*app.IdentityService
	test             *testing.T
	tokens           []string
	resources        []string
	snapshotTokens   []string
	snapshotResource []string
	admissions       int
	snapshots        int
	swapUser         bool
}

func (identity *memoryPreparationIdentity) ResolveMCPUser(ctx context.Context, token string, resources ...string) (domain.User, error) {
	identity.tokens = append(identity.tokens, token)
	identity.resources = append(identity.resources, strings.Join(resources, ","))
	user, err := identity.IdentityService.ResolveMCPUser(ctx, token, resources...)
	if identity.swapUser {
		user.ID = "different-user"
	}
	return user, err
}

func (identity *memoryPreparationIdentity) ResolveMCPUserForSnapshot(ctx context.Context, token string, resources ...string) (domain.User, error) {
	identity.test.Helper()
	if ctx.Value(memoryPreparationSnapshotKey{}) != true {
		identity.test.Fatal("snapshot authentication ran outside the preparer's snapshot callback")
	}
	if actor, system := inbound.RepositoryActor(ctx); actor != "" || system {
		identity.test.Fatal("snapshot authentication borrowed previous actor")
	}
	identity.snapshotTokens = append(identity.snapshotTokens, token)
	identity.snapshotResource = append(identity.snapshotResource, strings.Join(resources, ","))
	user, err := identity.IdentityService.ResolveMCPUser(ctx, token, resources...)
	if identity.swapUser {
		user.ID = "different-user"
	}
	return user, err
}

func (identity *memoryPreparationIdentity) AdmitRepositoryRead(ctx context.Context, repository, user string) (inbound.RepositoryReadPermit, error) {
	identity.admissions++
	if _, err := identity.IdentityService.AdmitRepositoryRead(ctx, repository, user); err != nil {
		return nil, err
	}
	return memoryPreparationPermitFunc(func(bound context.Context, requestedRepository, requestedUser string) error {
		identity.snapshots++
		if bound.Value(memoryPreparationSnapshotKey{}) != true || requestedRepository != repository || requestedUser != user {
			identity.test.Fatal("snapshot permit lost its repository, user or snapshot context")
		}
		_, err := identity.IdentityService.AdmitRepositoryRead(bound, requestedRepository, requestedUser)
		return err
	}), nil
}

type memoryPreparationAllowance struct {
	key      string
	limit    int
	window   time.Duration
	deadline time.Time
	now      time.Time
}

type memoryPreparationRuntime struct {
	outbound.RuntimeStore
	calls []memoryPreparationAllowance
	allow func(context.Context, string, int, time.Duration, time.Time) (bool, error)
}

func (runtime *memoryPreparationRuntime) AllowRequest(ctx context.Context, key string, limit int, window time.Duration, now time.Time) (bool, error) {
	deadline, _ := ctx.Deadline()
	runtime.calls = append(runtime.calls, memoryPreparationAllowance{key, limit, window, deadline, now})
	if runtime.allow != nil {
		return runtime.allow(ctx, key, limit, window, now)
	}
	return true, nil
}

func memoryPreparationBody(fixture *memoryAdmissionFixture) domain.MemoryReadPreparationRequest {
	return domain.MemoryReadPreparationRequest{Version: 1, RepoID: fixture.repo.ID,
		Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: fixture.snapshot.ID, CodeCommit: strings.Repeat("a", 40)},
		Content:   "claims", Cursor: "private-unpinned-cursor", DocumentIdentities: []domain.DocumentIdentity{domain.DocumentIdentityRootV1}}
}

func memoryPreparationRequest(test *testing.T, body domain.MemoryReadPreparationRequest, token string) *http.Request {
	test.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		test.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, domain.MemoryReadPreparationPath, bytes.NewReader(raw))
	for key, value := range remoteMCPHeaders(token) {
		request.Header.Set(key, value)
	}
	return request
}

func memoryPreparationHandler(test *testing.T, fixture *memoryAdmissionFixture, identity preparation.IdentityBackend, prepare memoryPreparationDemandFunc, runtime outbound.RuntimeStore) http.Handler {
	test.Helper()
	handler, err := preparation.NewMemoryPreparationHandler(fixture.backend, identity, prepare, runtime, "https://app.example.test", "https://memory.example.test")
	if err != nil {
		test.Fatal(err)
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if recorder, ok := writer.(*httptest.ResponseRecorder); ok {
			writer = &memoryPreparationDeadlineWriter{ResponseRecorder: recorder}
		}
		handler.ServeHTTP(writer, request)
	})
}

func assertMemoryPreparationFailure(test *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	test.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		test.Fatal("invalid error JSON", err, response.Body)
	}
	var failure map[string]any
	if err := json.Unmarshal(envelope["error"], &failure); err != nil {
		test.Fatal("missing error object", err, response.Body)
	}
	if response.Code != status || failure["code"] != code || len(envelope) != 1 {
		test.Fatalf("response=%d %s; want %d %s", response.Code, response.Body, status, code)
	}
	for key := range failure {
		if key != "code" && key != "retryable" {
			test.Fatalf("unexpected error field %q: %s", key, response.Body)
		}
	}
	if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Referrer-Policy") != "no-referrer" || len(response.Header().Values("Set-Cookie")) != 0 {
		test.Fatalf("unsafe response headers: %v", response.Header())
	}
}

func TestMemoryPreparationHandlerConstructor(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	prepare := memoryPreparationDemandFunc(func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error { return nil })
	for _, candidate := range []struct {
		name     string
		backend  preparation.ContextBackend
		identity preparation.IdentityBackend
		preparer inbound.PreparedMemoryDemand
		runtime  outbound.RuntimeStore
		public   string
		resource string
	}{
		{"missing context", nil, fixture.identity, prepare, fixture.storage, "https://app.test", "https://mcp.test"},
		{"missing identity", fixture.backend, nil, prepare, fixture.storage, "https://app.test", "https://mcp.test"},
		{"missing preparer", fixture.backend, fixture.identity, nil, fixture.storage, "https://app.test", "https://mcp.test"},
		{"missing runtime", fixture.backend, fixture.identity, prepare, nil, "https://app.test", "https://mcp.test"},
		{"missing public origin", fixture.backend, fixture.identity, prepare, fixture.storage, "", "https://mcp.test"},
		{"no resource fallback", fixture.backend, fixture.identity, prepare, fixture.storage, "https://app.test", ""},
		{"resource path", fixture.backend, fixture.identity, prepare, fixture.storage, "https://app.test", "https://mcp.test/mcp"},
		{"resource insecure", fixture.backend, fixture.identity, prepare, fixture.storage, "https://app.test", "http://mcp.test"},
		{"public credentials", fixture.backend, fixture.identity, prepare, fixture.storage, "https://secret@app.test", "https://mcp.test"},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			handler, err := preparation.NewMemoryPreparationHandler(candidate.backend, candidate.identity, candidate.preparer, candidate.runtime, candidate.public, candidate.resource)
			if err == nil || handler != nil {
				test.Fatalf("constructor accepted unsafe dependencies or origins: %v %v", handler, err)
			}
		})
	}
}

func TestMemoryPreparationHandlerAcceptedAndFreshCallbacks(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	identity := &memoryPreparationIdentity{IdentityService: fixture.identity, test: test}
	runtime := &memoryPreparationRuntime{}
	body := memoryPreparationBody(fixture)
	started := time.Now()
	var preparationContext context.Context
	calls := 0
	prepare := memoryPreparationDemandFunc(func(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) error {
		calls++
		preparationContext = ctx
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.After(started) || deadline.After(time.Now().Add(5*time.Second)) {
			test.Fatalf("missing bounded preparation deadline: %v", deadline)
		}
		if repo != body.RepoID || request != (domain.EffectiveMemoryRequest{Selection: body.Selection, Content: body.Content, Cursor: body.Cursor}) {
			test.Fatalf("preparation request changed: %s %+v", repo, request)
		}
		if !reflect.DeepEqual(inbound.DocumentIdentities(ctx), body.DocumentIdentities) || len(identity.tokens) != 1 || len(runtime.calls) != 2 || identity.admissions != 0 {
			test.Fatal("handler did not preserve identities or performed admission before the preparer")
		}
		if inbound.DocumentSnapshotReadAuthorizer(ctx) != nil {
			test.Fatal("handler fabricated snapshot authorization before admission")
		}
		authorize := inbound.DocumentReadAuthorizer(ctx)
		if authorize == nil {
			test.Fatal("missing admission authorization")
		}
		bound, err := authorize(ctx, repo)
		if err != nil {
			return err
		}
		snapshot := inbound.DocumentSnapshotReadAuthorizer(bound)
		if snapshot == nil {
			test.Fatal("missing fresh snapshot authorization")
		}
		bound, err = snapshot(context.WithValue(bound, memoryPreparationSnapshotKey{}, true), repo)
		if err != nil {
			return err
		}
		if actor, system := inbound.RepositoryActor(bound); actor != fixture.reader.ID || system {
			test.Fatal("snapshot authorization lost the authenticated user")
		}
		return nil
	})
	handler, err := preparation.NewMemoryPreparationHandler(fixture.backend, identity, prepare, runtime, " https://APP.example.test/ ", " https://MEMORY.example.test/ ")
	if err != nil {
		test.Fatal(err)
	}
	request := memoryPreparationRequest(test, body, fixture.token)
	request.Header.Set("Origin", "https://app.example.test")
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("X-CXT-Document-Identities", "untrusted-identity")
	request = request.WithContext(inbound.WithDocumentIdentities(request.Context(), []domain.DocumentIdentity{"untrusted-identity"}))
	response := &memoryPreparationDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || strings.TrimSpace(response.Body.String()) != `{"accepted":true}` || calls != 1 || identity.admissions != 1 || identity.snapshots != 1 {
		test.Fatalf("preparation response=%d %s calls=%d admissions=%d snapshots=%d", response.Code, response.Body, calls, identity.admissions, identity.snapshots)
	}
	if !reflect.DeepEqual(identity.tokens, []string{fixture.token, fixture.token}) || !reflect.DeepEqual(identity.resources, []string{"https://memory.example.test/mcp", "https://memory.example.test/mcp"}) ||
		!reflect.DeepEqual(identity.snapshotTokens, []string{fixture.token}) || !reflect.DeepEqual(identity.snapshotResource, []string{"https://memory.example.test/mcp"}) {
		test.Fatal("authentication callbacks did not preserve the MCP token and configured resource")
	}
	if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" || len(response.Header().Values("Set-Cookie")) != 0 {
		test.Fatalf("unsafe success headers: %v", response.Header())
	}
	if !errors.Is(preparationContext.Err(), context.Canceled) || fixture.backend.reads.Load() != 0 {
		test.Fatal("handler retained its context or performed a memory read")
	}
}

func TestMemoryPreparationHandlerBearerOnly(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	wrongAudience, err := fixture.identity.IssueMCPTokenPair(systemTestContext(), fixture.reader.ID, "wrong-client", "https://app.example.test/mcp")
	if err != nil {
		test.Fatal(err)
	}
	webIdentity := app.NewIdentityService(fixedVerifier{user: fixture.reader}, fixture.storage)
	_, webSession, err := webIdentity.Login(systemTestContext(), "idp-token", "browser")
	if err != nil {
		test.Fatal(err)
	}
	cliSession, err := fixture.identity.CreateCLIToken(systemTestContext(), fixture.reader.ID, "CLI")
	if err != nil {
		test.Fatal(err)
	}
	for _, candidate := range []struct {
		name   string
		bearer string
		cookie string
	}{
		{"missing", "", ""},
		{"wrong audience", wrongAudience.AccessToken, ""},
		{"web token", webSession.Token, ""},
		{"CLI token", cliSession.Token, ""},
		{"refresh token", wrongAudience.RefreshToken, ""},
		{"raw IDP token", "idp-token", ""},
		{"oversize bearer", strings.Repeat("b", 8193), ""},
		{"cookie only", "", fixture.token},
		{"web cookie only", "", webSession.Token},
		{"invalid bearer with valid cookie", "invalid-token", fixture.token},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			prepare := memoryPreparationDemandFunc(func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error {
				test.Fatal("unauthenticated request reached preparation")
				return nil
			})
			handler := memoryPreparationHandler(test, fixture, fixture.identity, prepare, &memoryPreparationRuntime{})
			request := memoryPreparationRequest(test, memoryPreparationBody(fixture), candidate.bearer)
			if candidate.cookie != "" {
				request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: candidate.cookie})
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertMemoryPreparationFailure(test, response, http.StatusUnauthorized, "unauthenticated")
		})
	}
}

func TestMemoryPreparationHandlerStrictJSON(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	valid, err := json.Marshal(memoryPreparationBody(fixture))
	if err != nil {
		test.Fatal(err)
	}
	for _, candidate := range []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"malformed", `{"version":`},
		{"null", `null`},
		{"array", `[]`},
		{"unknown field", strings.TrimSuffix(string(valid), "}") + `,"unexpected":true}`},
		{"unknown nested field", strings.Replace(string(valid), `"selection":{`, `"selection":{"unexpected":true,`, 1)},
		{"trailing object", string(valid) + `{}`},
		{"trailing null", string(valid) + `null`},
		{"trailing garbage", string(valid) + `secret-garbage`},
		{"invalid version", strings.Replace(string(valid), `"version":1`, `"version":2`, 1)},
		{"invalid repository", strings.Replace(string(valid), string(fixture.repo.ID), "not-a-hash", 1)},
		{"empty cursor", strings.Replace(string(valid), "private-unpinned-cursor", "", 1)},
		{"unsupported identity", strings.Replace(string(valid), string(domain.DocumentIdentityRootV1), "future-identity", 1)},
		{"oversize field", strings.Replace(string(valid), "private-unpinned-cursor", strings.Repeat("x", domain.MaxMemoryReadPreparationBytes), 1)},
		{"oversize whitespace", string(valid) + strings.Repeat(" ", domain.MaxMemoryReadPreparationBytes)},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			runtime := &memoryPreparationRuntime{}
			handler := memoryPreparationHandler(test, fixture, fixture.identity, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error {
				test.Fatal("invalid request reached preparation")
				return nil
			}, runtime)
			request := httptest.NewRequest(http.MethodPost, domain.MemoryReadPreparationPath, strings.NewReader(candidate.body))
			request.ContentLength = -1
			for key, value := range remoteMCPHeaders(fixture.token) {
				request.Header.Set(key, value)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertMemoryPreparationFailure(test, response, http.StatusUnprocessableEntity, "validation")
			if len(runtime.calls) != 1 {
				test.Fatal("invalid payload created a per-user/repository allowance key")
			}
		})
	}
	for _, mediaType := range []string{"", "text/plain", "application/json; broken"} {
		test.Run("content type "+mediaType, func(test *testing.T) {
			handler := memoryPreparationHandler(test, fixture, fixture.identity, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error {
				test.Fatal("non-JSON request reached preparation")
				return nil
			}, &memoryPreparationRuntime{})
			request := memoryPreparationRequest(test, memoryPreparationBody(fixture), fixture.token)
			request.Header.Set("Content-Type", mediaType)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertMemoryPreparationFailure(test, response, http.StatusUnprocessableEntity, "validation")
		})
	}
}

func TestMemoryPreparationHandlerOnlyPreparationRoute(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	identity := &memoryPreparationIdentity{IdentityService: fixture.identity, test: test}
	runtime := &memoryPreparationRuntime{}
	handler := memoryPreparationHandler(test, fixture, identity, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error {
		test.Fatal("unexpected route reached preparation")
		return nil
	}, runtime)
	for _, path := range []string{"/", "/mcp", "/oauth/register", "/oauth/authorize", "/oauth/token", "/oauth/revoke", "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server", "/api/v1/repos", domain.MemoryReadPreparationPath + "/"} {
		response := mcpRequest(test, handler, http.MethodPost, path, nil, remoteMCPHeaders(fixture.token))
		assertMemoryPreparationFailure(test, response, http.StatusNotFound, "not_found")
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodPut, http.MethodOptions} {
		response := mcpRequest(test, handler, method, domain.MemoryReadPreparationPath, nil, remoteMCPHeaders(fixture.token))
		assertMemoryPreparationFailure(test, response, http.StatusMethodNotAllowed, "method_not_allowed")
		if response.Header().Get("Allow") != http.MethodPost {
			test.Fatal("missing POST-only Allow header")
		}
	}
	for _, origin := range []string{"https://evil.example.test", "https://memory.example.test", "null"} {
		request := memoryPreparationRequest(test, memoryPreparationBody(fixture), fixture.token)
		request.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		assertMemoryPreparationFailure(test, response, http.StatusForbidden, "forbidden")
	}
	if len(runtime.calls) != 0 || len(identity.tokens) != 0 {
		test.Fatal("unexpected route or origin reached authentication or allowance storage")
	}
}

func TestMemoryPreparationHandlerFreshRevocationAndPermissions(test *testing.T) {
	for _, phase := range []string{"admission", "snapshot"} {
		for _, mutation := range []string{"token", "permission", "identity"} {
			test.Run(phase+"/"+mutation, func(test *testing.T) {
				fixture := newMemoryAdmissionFixture(test)
				identity := &memoryPreparationIdentity{IdentityService: fixture.identity, test: test}
				mutate := func() {
					switch mutation {
					case "token":
						if err := fixture.identity.RevokeMCPToken(systemTestContext(), fixture.token, "memory-admission-client"); err != nil {
							test.Fatal(err)
						}
					case "permission":
						if err := fixture.identity.RemoveMember(systemTestContext(), fixture.owner.ID, fixture.repository.ID, fixture.reader.ID); err != nil {
							test.Fatal(err)
						}
					case "identity":
						identity.swapUser = true
					}
				}
				completed := false
				prepare := memoryPreparationDemandFunc(func(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) error {
					if len(identity.tokens) != 1 {
						test.Fatal("handler authenticated after initial admission outside the preparer")
					}
					if phase == "admission" {
						mutate()
					}
					return fixture.backend.Service.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
						if phase == "snapshot" {
							mutate()
						}
						authorize := inbound.DocumentSnapshotReadAuthorizer(bound)
						if authorize == nil {
							test.Fatal("missing fresh snapshot authorizer")
						}
						if _, err := authorize(context.WithValue(bound, memoryPreparationSnapshotKey{}, true), repo); err != nil {
							return err
						}
						completed = true
						return nil
					})
				})
				handler := memoryPreparationHandler(test, fixture, identity, prepare, &memoryPreparationRuntime{})
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, memoryPreparationRequest(test, memoryPreparationBody(fixture), fixture.token))
				status, code := http.StatusUnauthorized, "unauthenticated"
				if mutation == "permission" {
					status, code = http.StatusForbidden, "forbidden"
				}
				assertMemoryPreparationFailure(test, response, status, code)
				if completed || len(identity.tokens) != 2 || fixture.backend.reads.Load() != 0 {
					test.Fatal("revoked request passed fresh authorization")
				}
				if phase == "snapshot" && len(identity.snapshotTokens) != 1 {
					test.Fatal("snapshot revocation did not use fresh MCP identity resolution")
				}
			})
		}
	}
}

func TestMemoryPreparationHandlerRateLimitsFailClosed(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	for _, phase := range []int{1, 2} {
		for _, storageFailure := range []bool{false, true} {
			test.Run(fmt.Sprintf("phase=%d/storage_failure=%t", phase, storageFailure), func(test *testing.T) {
				identity := &memoryPreparationIdentity{IdentityService: fixture.identity, test: test}
				runtime := &memoryPreparationRuntime{}
				runtime.allow = func(context.Context, string, int, time.Duration, time.Time) (bool, error) {
					if len(runtime.calls) != phase {
						return true, nil
					}
					if storageFailure {
						return false, errors.New("private-storage-details " + fixture.token)
					}
					return false, nil
				}
				handler := memoryPreparationHandler(test, fixture, identity, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error {
					test.Fatal("rate-limited request reached preparation")
					return nil
				}, runtime)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, memoryPreparationRequest(test, memoryPreparationBody(fixture), fixture.token))
				status, code := http.StatusTooManyRequests, "resource_overloaded"
				if storageFailure {
					status, code = http.StatusServiceUnavailable, "memory_preparation_unavailable"
				} else if response.Header().Get("Retry-After") != "60" {
					test.Fatal("rate limit missing Retry-After")
				}
				assertMemoryPreparationFailure(test, response, status, code)
				if len(runtime.calls) != phase || len(identity.tokens) != phase-1 || strings.Contains(response.Body.String(), fixture.token) {
					test.Fatal("rate limit failed closed or exposed storage details")
				}
			})
		}
	}
}

func TestMemoryPreparationHandlerSharedBoundedRateKeys(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	rotated, err := fixture.identity.IssueMCPTokenPair(systemTestContext(), fixture.reader.ID, "second-client", "https://memory.example.test/mcp")
	if err != nil {
		test.Fatal(err)
	}
	owner, err := fixture.identity.IssueMCPTokenPair(systemTestContext(), fixture.owner.ID, "owner-client", "https://memory.example.test/mcp")
	if err != nil {
		test.Fatal(err)
	}
	runtime := &memoryPreparationRuntime{}
	runtime.allow = func(ctx context.Context, key string, limit int, window time.Duration, now time.Time) (bool, error) {
		if strings.HasSuffix(key, ":global") {
			return true, nil
		}
		return fixture.storage.AllowRequest(ctx, key, 1, time.Hour, now)
	}
	calls := 0
	prepare := memoryPreparationDemandFunc(func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error { calls++; return nil })
	body := memoryPreparationBody(fixture)
	keys := make([]string, 0, 4)
	for index, token := range []string{fixture.token, rotated.AccessToken, fixture.token, owner.AccessToken} {
		if index == 2 {
			body.RepoID = domain.HashContent([]byte("different repository"))
		}
		handler := memoryPreparationHandler(test, fixture, fixture.identity, prepare, runtime)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, memoryPreparationRequest(test, body, token))
		if index == 1 {
			assertMemoryPreparationFailure(test, response, http.StatusTooManyRequests, "resource_overloaded")
		} else if response.Code != http.StatusAccepted {
			test.Fatalf("unexpected preparation response=%d %s", response.Code, response.Body)
		}
		keys = append(keys, runtime.calls[len(runtime.calls)-1].key)
	}
	if keys[0] != keys[1] || keys[0] == keys[2] || keys[2] == keys[3] || calls != 3 {
		test.Fatalf("allowance not shared by user/repository independently of bearer: %v calls=%d", keys, calls)
	}
	for index, allowance := range runtime.calls {
		if allowance.deadline.IsZero() || allowance.deadline.After(time.Now().Add(2*time.Second)) || !allowance.now.IsZero() || allowance.window != time.Minute {
			test.Fatalf("unbounded allowance request: %+v", allowance)
		}
		if strings.Contains(allowance.key, fixture.token) || strings.Contains(allowance.key, rotated.AccessToken) || strings.Contains(allowance.key, fixture.reader.ID) || len(allowance.key) > 100 {
			test.Fatalf("unbounded or secret-bearing allowance key: %q", allowance.key)
		}
		if index%2 == 0 {
			if allowance.key != "mcp:memory-preparation:global" || allowance.limit != 600 {
				test.Fatal("global allowance not shared")
			}
		} else if allowance.limit != 30 || domain.ValidateContentHash(domain.ContentHash(strings.TrimPrefix(allowance.key, "mcp:memory-preparation:scope:"))) != nil {
			test.Fatal("per-user/repository allowance key is not a fixed hash")
		}
	}
}

func TestMemoryPreparationHandlerSanitizedFailures(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	for _, candidate := range []struct {
		cause  error
		status int
		code   string
	}{
		{domain.ErrUnauthorized, 401, "unauthenticated"},
		{domain.ErrForbidden, 403, "forbidden"},
		{domain.ErrNotFound, 404, "not_found"},
		{domain.ErrValidation, 422, "validation"},
		{domain.ErrMemoryProjectionLimit, 422, "memory_projection_limit"},
		{domain.ErrEffectiveMemoryCursorStale, 409, "memory_cursor_stale"},
		{domain.ErrMemoryCaptureExpired, 409, "memory_capture_expired"},
		{domain.ErrCatalogResetRequired, 409, "catalog_reset_required"},
		{domain.ErrConflict, 409, "conflict"},
		{domain.ErrDocumentIdentityUpgradeRequired, 409, "document_identity_upgrade_required"},
		{domain.ErrResourceOverloaded, 429, "resource_overloaded"},
		{domain.ErrCatalogUnsupported, 503, "prepared_memory_unavailable"},
		{domain.ErrMemoryPreparationUnavailable, 503, "memory_preparation_unavailable"},
		{domain.ErrMemoryReadNeedsAttention, 503, "memory_read_needs_attention"},
		{domain.ErrBreakGlassAuditUnavailable, 503, "audit_unavailable"},
		{domain.ErrIntegrity, 503, "integrity_violation"},
		{context.DeadlineExceeded, 503, "deadline_exceeded"},
		{context.Canceled, 503, "canceled"},
		{&domain.MemoryReadPending{Cursor: "secret-retry-cursor"}, 503, "memory_read_not_ready"},
		{&preparedMemoryToolError{Code: "memory_read_not_ready", Message: "secret-message", RetryCursor: "secret-retry-cursor", cause: domain.ErrMemoryReadNotReady}, 503, "memory_read_not_ready"},
		{errors.Join(&domain.MemoryReadPending{Cursor: "secret-retry-cursor"}, errors.New("secret-error")), 503, "unavailable"},
		{errors.Join(&domain.MemoryReadPending{Cursor: "secret-retry-cursor"}, fmt.Errorf("secret-wrapper: %w", errors.New("secret-error"))), 503, "unavailable"},
		{errors.Join(&domain.MemoryReadPending{Cursor: "secret-retry-cursor"}, errors.New("secret-error"), domain.ErrUnauthorized), 401, "unauthenticated"},
		{errors.Join(&domain.MemoryReadPending{Cursor: "secret-retry-cursor"}, errors.New("secret-error"), domain.ErrForbidden), 403, "forbidden"},
		{errors.Join(&domain.MemoryReadPending{Cursor: "secret-retry-cursor"}, errors.New("secret-error"), domain.ErrIntegrity), 503, "integrity_violation"},
		{errors.New("secret-error"), 503, "unavailable"},
	} {
		test.Run(candidate.code, func(test *testing.T) {
			handler := memoryPreparationHandler(test, fixture, fixture.identity, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error {
				return fmt.Errorf("private content %s: %w", fixture.token, candidate.cause)
			}, &memoryPreparationRuntime{})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, memoryPreparationRequest(test, memoryPreparationBody(fixture), fixture.token))
			assertMemoryPreparationFailure(test, response, candidate.status, candidate.code)
			failure := preparedMemoryFailure(candidate.cause).(*preparedMemoryToolError)
			if !strings.Contains(response.Body.String(), fmt.Sprintf(`"retryable":%t`, failure.Retryable)) {
				test.Fatal("response lost prepared-memory retryability")
			}
			for _, secret := range []string{fixture.token, "private", "secret", "cursor", "content"} {
				if secret == "cursor" && candidate.code == "memory_cursor_stale" {
					continue
				}
				if strings.Contains(response.Body.String(), secret) {
					test.Fatalf("failure leaked %q: %s", secret, response.Body)
				}
			}
		})
	}
}

func TestMemoryPreparationHandlerCallerDeadlineAndCancellation(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	for _, cancelBefore := range []bool{false, true} {
		test.Run(fmt.Sprintf("already_canceled=%t", cancelBefore), func(test *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			deadline, _ := ctx.Deadline()
			if cancelBefore {
				cancel()
			}
			calls := 0
			handler := memoryPreparationHandler(test, fixture, fixture.identity, func(bound context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) error {
				calls++
				if actual, _ := bound.Deadline(); !actual.Equal(deadline) {
					test.Fatal("handler extended the caller's shorter deadline")
				}
				cancel()
				return nil
			}, &memoryPreparationRuntime{})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, memoryPreparationRequest(test, memoryPreparationBody(fixture), fixture.token).WithContext(ctx))
			assertMemoryPreparationFailure(test, response, http.StatusServiceUnavailable, "canceled")
			if cancelBefore && calls != 0 || !cancelBefore && calls != 1 {
				test.Fatalf("unexpected preparation calls: %d", calls)
			}
		})
	}
}

func TestMemoryPreparationHandlerPartialBodyDeadline(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	for _, candidate := range []struct {
		name   string
		token  string
		status int
		code   string
	}{
		{"authenticated", fixture.token, http.StatusServiceUnavailable, "deadline_exceeded"},
		{"unauthenticated", "invalid-token", http.StatusUnauthorized, "unauthenticated"},
		{"overloaded", fixture.token, http.StatusTooManyRequests, "resource_overloaded"},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			runtime := &memoryPreparationRuntime{allow: func(context.Context, string, int, time.Duration, time.Time) (bool, error) {
				return candidate.name != "overloaded", nil
			}}
			handler := memoryPreparationHandler(test, fixture, fixture.identity, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error {
				test.Error("partial body reached preparation")
				return nil
			}, runtime)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				ctx, cancel := context.WithTimeout(request.Context(), 100*time.Millisecond)
				defer cancel()
				handler.ServeHTTP(writer, request.WithContext(ctx))
			}))
			defer server.Close()
			connection, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
			if err != nil {
				test.Fatal(err)
			}
			defer connection.Close()
			if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				test.Fatal(err)
			}
			started := time.Now()
			if _, err := fmt.Fprintf(connection, "POST %s HTTP/1.1\r\nHost: example.test\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 1024\r\n\r\n{\"version\":1", domain.MemoryReadPreparationPath, candidate.token); err != nil {
				test.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(connection), nil)
			if err != nil {
				test.Fatal("partial-body deadline did not produce a bounded response", err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				test.Fatal(err)
			}
			if response.StatusCode != candidate.status || !strings.Contains(string(body), `"code":"`+candidate.code+`"`) || time.Since(started) > time.Second {
				test.Fatalf("stalled body response=%d %s elapsed=%v", response.StatusCode, body, time.Since(started))
			}
		})
	}
}

type memoryPreparationDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	failure   error
}

func (writer *memoryPreparationDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	writer.deadlines = append(writer.deadlines, deadline)
	return writer.failure
}

func TestMemoryPreparationHandlerRestoresBodyDeadlineAfterEOF(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	for _, readTimeout := range []time.Duration{0, 50 * time.Millisecond} {
		test.Run(readTimeout.String(), func(test *testing.T) {
			response := &memoryPreparationDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			ctx = context.WithValue(ctx, http.ServerContextKey, &http.Server{ReadTimeout: readTimeout})
			deadline, _ := ctx.Deadline()
			handler := memoryPreparationHandler(test, fixture, fixture.identity, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error {
				if readTimeout == 0 {
					if len(response.deadlines) != 2 || !response.deadlines[0].Equal(deadline) || !response.deadlines[1].IsZero() {
						test.Fatal("body deadline was not reset after EOF and before preparation", response.deadlines)
					}
				} else if len(response.deadlines) != 1 || response.deadlines[0].IsZero() || response.deadlines[0].After(time.Now().Add(readTimeout)) {
					test.Fatal("body deadline disabled or extended the server read timeout", response.deadlines)
				}
				return nil
			}, &memoryPreparationRuntime{})
			handler.ServeHTTP(response, memoryPreparationRequest(test, memoryPreparationBody(fixture), fixture.token).WithContext(ctx))
			if response.Code != http.StatusAccepted {
				test.Fatalf("deadline response=%d %s", response.Code, response.Body)
			}
		})
	}
}

func TestMemoryPreparationHandlerBodyDeadlineFailure(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	for _, failure := range []error{errors.New("secret-connection-error"), http.ErrNotSupported} {
		response := &memoryPreparationDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), failure: failure}
		handler := memoryPreparationHandler(test, fixture, fixture.identity, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error {
			test.Fatal("request without a network body deadline reached preparation")
			return nil
		}, &memoryPreparationRuntime{})
		request := memoryPreparationRequest(test, memoryPreparationBody(fixture), fixture.token)
		request = request.WithContext(context.WithValue(request.Context(), http.ServerContextKey, &http.Server{}))
		handler.ServeHTTP(response, request)
		assertMemoryPreparationFailure(test, response.ResponseRecorder, http.StatusServiceUnavailable, "memory_preparation_unavailable")
	}
	handler, err := preparation.NewMemoryPreparationHandler(fixture.backend, fixture.identity, memoryPreparationDemandFunc(func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error {
		test.Fatal("unsupported response controller reached preparation")
		return nil
	}), &memoryPreparationRuntime{}, "https://app.example.test", "https://memory.example.test")
	if err != nil {
		test.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, memoryPreparationRequest(test, memoryPreparationBody(fixture), fixture.token))
	assertMemoryPreparationFailure(test, response, http.StatusServiceUnavailable, "memory_preparation_unavailable")
}

func TestMemoryPreparationHandlerClientErrorRoundTrip(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	pending := &domain.MemoryReadPending{Cursor: "secret-retry-cursor"}
	unknown := errors.New("private-unknown-preparation-failure")
	for _, cause := range []error{
		nil,
		domain.ErrUnauthorized,
		domain.ErrForbidden,
		domain.ErrBreakGlassAuditUnavailable,
		domain.ErrIntegrity,
		domain.ErrMemoryReadNeedsAttention,
		domain.ErrEffectiveMemoryCursorStale,
		domain.ErrMemoryCaptureExpired,
		domain.ErrCatalogResetRequired,
		domain.ErrCatalogUnsupported,
		domain.ErrDocumentIdentityUpgradeRequired,
		domain.ErrMemoryPreparationUnavailable,
		domain.ErrConflict,
		context.Canceled,
		domain.ErrValidation,
		domain.ErrNotFound,
		domain.ErrMemoryProjectionLimit,
		domain.ErrResourceOverloaded,
		context.DeadlineExceeded,
		domain.ErrMemoryReadNotReady,
		unknown,
		fmt.Errorf("private pending wrapper: %w", pending),
		errors.Join(pending, errors.Join(domain.ErrMemoryReadNotReady, pending)),
		errors.Join(pending, unknown),
		errors.Join(unknown, pending),
		errors.Join(pending, fmt.Errorf("private unknown wrapper: %w", unknown)),
		fmt.Errorf("private nested wrapper: %w", errors.Join(pending, errors.Join(domain.ErrMemoryReadNotReady, unknown))),
		errors.Join(pending, unknown, domain.ErrUnauthorized),
		errors.Join(pending, unknown, domain.ErrForbidden),
		errors.Join(pending, unknown, domain.ErrIntegrity),
		errors.Join(pending, unknown, domain.ErrMemoryReadNeedsAttention),
		errors.Join(pending, unknown, domain.ErrMemoryCaptureExpired),
		errors.Join(pending, unknown, domain.ErrEffectiveMemoryCursorStale),
	} {
		name := "accepted"
		if cause != nil {
			name = cause.Error()
		}
		test.Run(name, func(test *testing.T) {
			handler := memoryPreparationHandler(test, fixture, fixture.identity, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error {
				if cause == nil {
					return nil
				}
				return fmt.Errorf("private request details %s: %w", fixture.token, cause)
			}, &memoryPreparationRuntime{})
			server := httptest.NewServer(handler)
			defer server.Close()
			client, err := memorypreparation.NewClient(server.URL)
			if err != nil {
				test.Fatal(err)
			}
			err = client.RequestMemoryPreparation(context.Background(), fixture.token, memoryPreparationBody(fixture))
			if cause == nil {
				if err != nil {
					test.Fatal("accepted preparation failed round trip", err)
				}
				return
			}
			if err == nil {
				test.Fatal("preparation error became an accepted result")
			}
			expected := preparedMemoryFailure(cause).(*preparedMemoryToolError)
			actual := preparedMemoryFailure(err).(*preparedMemoryToolError)
			if expected.Code != "unavailable" && !errors.Is(cause, err) {
				test.Fatalf("round trip lost %s: %v", expected.Code, err)
			}
			if actual.Retryable != expected.Retryable || actual.Code != expected.Code {
				test.Fatalf("round trip changed code/retryability: got %s/%t want %s/%t", actual.Code, actual.Retryable, expected.Code, expected.Retryable)
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), fixture.token) {
				test.Fatalf("round trip exposed private failure: %v", err)
			}
		})
	}
}
