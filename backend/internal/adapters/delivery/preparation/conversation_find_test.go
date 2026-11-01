package preparation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type conversationFindDemandFunc func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) error

func (prepare conversationFindDemandFunc) RequestConversationFind(ctx context.Context, repo, hash domain.ContentHash, request domain.ConversationFindRequest) error {
	return prepare(ctx, repo, hash, request)
}

type conversationFindContext struct {
	*app.Service
	lookups int
	get     func(context.Context, domain.ContentHash) (domain.Repo, error)
}

func (backend *conversationFindContext) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	backend.lookups++
	if backend.get != nil {
		return backend.get(ctx, repo)
	}
	return backend.Service.GetRepo(ctx, repo)
}

type conversationFindSnapshotKey struct{}

type conversationFindPermitFunc func(context.Context, string, string) error

func (permit conversationFindPermitFunc) AuthorizeSnapshot(ctx context.Context, repository, user string) error {
	return permit(ctx, repository, user)
}

type conversationFindIdentity struct {
	*app.IdentityService
	test              *testing.T
	tokens            []string
	resources         []string
	snapshotTokens    []string
	snapshotResources []string
	admissions        int
	snapshots         int
	swapUser          bool
	nilPermit         bool
}

func (identity *conversationFindIdentity) ResolveMCPUser(ctx context.Context, token string, resources ...string) (domain.User, error) {
	identity.tokens = append(identity.tokens, token)
	identity.resources = append(identity.resources, strings.Join(resources, ","))
	user, err := identity.IdentityService.ResolveMCPUser(ctx, token, resources...)
	if identity.swapUser {
		user.ID = "different-user"
	}
	return user, err
}

func (identity *conversationFindIdentity) ResolveMCPUserForSnapshot(ctx context.Context, token string, resources ...string) (domain.User, error) {
	identity.test.Helper()
	if ctx.Value(conversationFindSnapshotKey{}) != true {
		identity.test.Fatal("snapshot authentication ran outside the application's snapshot")
	}
	if actor, system := inbound.RepositoryActor(ctx); actor != "" || system {
		identity.test.Fatal("snapshot authentication borrowed an earlier actor")
	}
	identity.snapshotTokens = append(identity.snapshotTokens, token)
	identity.snapshotResources = append(identity.snapshotResources, strings.Join(resources, ","))
	user, err := identity.IdentityService.ResolveMCPUser(ctx, token, resources...)
	if identity.swapUser {
		user.ID = "different-user"
	}
	return user, err
}

func (identity *conversationFindIdentity) AdmitRepositoryRead(ctx context.Context, repository, user string) (inbound.RepositoryReadPermit, error) {
	identity.admissions++
	if _, err := identity.IdentityService.AdmitRepositoryRead(ctx, repository, user); err != nil {
		return nil, err
	}
	if identity.nilPermit {
		return nil, nil
	}
	return conversationFindPermitFunc(func(bound context.Context, requestedRepository, requestedUser string) error {
		identity.snapshots++
		if bound.Value(conversationFindSnapshotKey{}) != true || requestedRepository != repository || requestedUser != user {
			identity.test.Fatal("snapshot permit lost its snapshot, repository or user")
		}
		_, err := identity.IdentityService.AdmitRepositoryRead(bound, requestedRepository, requestedUser)
		return err
	}), nil
}

type conversationFindAllowance struct {
	key      string
	limit    int
	window   time.Duration
	deadline time.Time
	now      time.Time
}

type conversationFindRuntime struct {
	outbound.RuntimeStore
	calls []conversationFindAllowance
	allow func(context.Context, string, int, time.Duration, time.Time) (bool, error)
}

func (runtime *conversationFindRuntime) AllowRequest(ctx context.Context, key string, limit int, window time.Duration, now time.Time) (bool, error) {
	deadline, _ := ctx.Deadline()
	runtime.calls = append(runtime.calls, conversationFindAllowance{key, limit, window, deadline, now})
	if runtime.allow != nil {
		return runtime.allow(ctx, key, limit, window, now)
	}
	return true, nil
}

type conversationFindDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	set       func(time.Time) error
}

func (writer *conversationFindDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	writer.deadlines = append(writer.deadlines, deadline)
	if writer.set != nil {
		return writer.set(deadline)
	}
	return nil
}

type conversationFindFixture struct {
	backend    *conversationFindContext
	identity   *conversationFindIdentity
	runtime    *conversationFindRuntime
	owner      domain.User
	reader     domain.User
	repository domain.Repository
	token      string
	body       domain.ConversationFindPreparationRequest
}

func newConversationFindFixture(test *testing.T) *conversationFindFixture {
	test.Helper()
	ctx := inbound.WithSystemActor(context.Background())
	storage := store.NewFSStore(test.TempDir())
	identity := app.NewIdentityService(nil, storage)
	owner := domain.User{ID: "find-owner", Username: "find-owner", Email: "owner@example.test"}
	reader := domain.User{ID: "find-reader", Username: "find-reader", Email: "reader@example.test"}
	for _, user := range []domain.User{owner, reader} {
		if err := storage.UpsertUser(ctx, user); err != nil {
			test.Fatal(err)
		}
	}
	repository, err := identity.CreateRepository(ctx, owner, "Conversation-find")
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.AddMember(ctx, domain.Membership{RepositoryID: repository.ID, UserID: reader.ID, Role: domain.RoleViewer}); err != nil {
		test.Fatal(err)
	}
	repo := domain.Repo{ID: domain.HashContent([]byte(repository.ID)), RepositoryID: repository.ID, DefaultBranch: "main"}
	if _, err := storage.PutRepo(ctx, repo); err != nil {
		test.Fatal(err)
	}
	pair, err := identity.IssueMCPTokenPair(ctx, reader.ID, "find-client", "https://mcp.example.test/mcp")
	if err != nil {
		test.Fatal(err)
	}
	return &conversationFindFixture{
		backend:  &conversationFindContext{Service: app.NewService(storage, storage, nil, nil, storage)},
		identity: &conversationFindIdentity{IdentityService: identity, test: test}, runtime: &conversationFindRuntime{},
		owner: owner, reader: reader, repository: repository, token: pair.AccessToken,
		body: domain.ConversationFindPreparationRequest{Version: 1, RepoID: repo.ID, Hash: domain.HashContent([]byte("private document")),
			Find: domain.ConversationFindRequest{Query: "private 검색", Cursor: "private-cursor"}, DocumentIdentities: []domain.DocumentIdentity{domain.DocumentIdentityRootV1}},
	}
}

func (fixture *conversationFindFixture) handler(test *testing.T, prepare conversationFindDemandFunc) http.Handler {
	test.Helper()
	handler, err := NewConversationFindPreparationHandler(fixture.backend, fixture.identity, prepare, fixture.runtime, " https://APP.example.test/ ", " https://MCP.example.test/ ")
	if err != nil {
		test.Fatal(err)
	}
	return handler
}

func (fixture *conversationFindFixture) rawRequest(body io.Reader) *http.Request {
	request := httptest.NewRequest(http.MethodPost, domain.ConversationFindPreparationPath, body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+fixture.token)
	return request
}

func (fixture *conversationFindFixture) request(test *testing.T) *http.Request {
	test.Helper()
	raw, err := json.Marshal(fixture.body)
	if err != nil {
		test.Fatal(err)
	}
	return fixture.rawRequest(bytes.NewReader(raw))
}

func assertConversationFindResponse(test *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	test.Helper()
	if response.Code != status || response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Referrer-Policy") != "no-referrer" || len(response.Header().Values("Set-Cookie")) != 0 {
		test.Fatalf("unexpected response: %d %s %v", response.Code, response.Body, response.Header())
	}
	if code == "" {
		if strings.TrimSpace(response.Body.String()) != `{"accepted":true}` {
			test.Fatalf("unexpected success envelope: %s", response.Body)
		}
		return
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		test.Fatal(err)
	}
	var failure map[string]any
	if err := json.Unmarshal(envelope["error"], &failure); err != nil || len(envelope) != 1 || failure["code"] != code || len(failure) != 2 {
		test.Fatalf("unexpected failure envelope: %s", response.Body)
	}
	if _, ok := failure["retryable"].(bool); !ok {
		test.Fatalf("missing retryability: %s", response.Body)
	}
}

func TestConversationFindPreparationConstructor(test *testing.T) {
	fixture := newConversationFindFixture(test)
	prepare := conversationFindDemandFunc(func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) error {
		return nil
	})
	var nilContext *conversationFindContext
	var nilIdentity *conversationFindIdentity
	var nilRuntime *conversationFindRuntime
	var nilDemand conversationFindDemandFunc
	for _, candidate := range []struct {
		name     string
		backend  ContextBackend
		identity IdentityBackend
		prepare  inbound.ConversationFindDemand
		runtime  outbound.RuntimeStore
		public   string
		resource string
	}{
		{"nil context", nil, fixture.identity, prepare, fixture.runtime, "https://app.test", "https://mcp.test"},
		{"typed nil context", nilContext, fixture.identity, prepare, fixture.runtime, "https://app.test", "https://mcp.test"},
		{"nil identity", fixture.backend, nil, prepare, fixture.runtime, "https://app.test", "https://mcp.test"},
		{"typed nil identity", fixture.backend, nilIdentity, prepare, fixture.runtime, "https://app.test", "https://mcp.test"},
		{"nil demand", fixture.backend, fixture.identity, nil, fixture.runtime, "https://app.test", "https://mcp.test"},
		{"typed nil demand", fixture.backend, fixture.identity, nilDemand, fixture.runtime, "https://app.test", "https://mcp.test"},
		{"nil runtime", fixture.backend, fixture.identity, prepare, nil, "https://app.test", "https://mcp.test"},
		{"typed nil runtime", fixture.backend, fixture.identity, prepare, nilRuntime, "https://app.test", "https://mcp.test"},
		{"empty origin", fixture.backend, fixture.identity, prepare, fixture.runtime, "", "https://mcp.test"},
		{"public HTTP", fixture.backend, fixture.identity, prepare, fixture.runtime, "http://app.test", "https://mcp.test"},
		{"resource HTTP", fixture.backend, fixture.identity, prepare, fixture.runtime, "https://app.test", "http://mcp.test"},
		{"resource path", fixture.backend, fixture.identity, prepare, fixture.runtime, "https://app.test", "https://mcp.test/mcp"},
		{"credentials", fixture.backend, fixture.identity, prepare, fixture.runtime, "https://secret@app.test", "https://mcp.test"},
		{"query", fixture.backend, fixture.identity, prepare, fixture.runtime, "https://app.test?secret", "https://mcp.test"},
		{"fragment", fixture.backend, fixture.identity, prepare, fixture.runtime, "https://app.test", "https://mcp.test/#secret"},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			handler, err := NewConversationFindPreparationHandler(candidate.backend, candidate.identity, candidate.prepare, candidate.runtime, candidate.public, candidate.resource)
			if handler != nil || err == nil {
				test.Fatalf("unsafe configuration accepted: %v %v", handler, err)
			}
		})
	}
	if _, err := NewConversationFindPreparationHandler(fixture.backend, fixture.identity, prepare, fixture.runtime, "http://localhost:3000", "http://127.0.0.1:4000"); err != nil {
		test.Fatal("loopback development origins rejected", err)
	}
}

func TestConversationFindPreparationAcceptedAndDelegatedAuthorization(test *testing.T) {
	fixture := newConversationFindFixture(test)
	var preparationContext context.Context
	calls := 0
	handler := fixture.handler(test, func(ctx context.Context, repo, hash domain.ContentHash, request domain.ConversationFindRequest) error {
		calls++
		preparationContext = ctx
		if repo != fixture.body.RepoID || hash != fixture.body.Hash || request != fixture.body.Find || !reflect.DeepEqual(inbound.DocumentIdentities(ctx), fixture.body.DocumentIdentities) {
			test.Fatal("document demand lost its bounded request or capabilities")
		}
		deadline, bounded := ctx.Deadline()
		if !bounded || deadline.After(time.Now().Add(5*time.Second)) || fixture.backend.lookups != 0 || fixture.identity.admissions != 0 || len(fixture.runtime.calls) != 2 {
			test.Fatal("handler performed application admission or lost its deadline/rate limits")
		}
		if inbound.DocumentSnapshotReadAuthorizer(ctx) != nil {
			test.Fatal("snapshot authorization exists before application admission")
		}
		authorize := inbound.DocumentReadAuthorizer(ctx)
		if authorize == nil {
			test.Fatal("missing delegated admission authorizer")
		}
		bound, err := authorize(ctx, repo)
		if err != nil {
			return err
		}
		snapshot := inbound.DocumentSnapshotReadAuthorizer(bound)
		if snapshot == nil {
			test.Fatal("missing delegated snapshot authorizer")
		}
		bound, err = snapshot(context.WithValue(bound, conversationFindSnapshotKey{}, true), repo)
		if err == nil {
			if actor, system := inbound.RepositoryActor(bound); actor != fixture.reader.ID || system {
				test.Fatal("snapshot authorization lost its authenticated actor")
			}
		}
		return err
	})
	request := fixture.request(test)
	request.Header.Set("Origin", "https://app.example.test")
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("X-CXT-Document-Identities", "untrusted-identity")
	request = request.WithContext(inbound.WithDocumentIdentities(request.Context(), []domain.DocumentIdentity{"untrusted-identity"}))
	response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(response, request)
	assertConversationFindResponse(test, response.ResponseRecorder, http.StatusAccepted, "")
	if calls != 1 || fixture.backend.lookups != 2 || fixture.identity.admissions != 1 || fixture.identity.snapshots != 1 || !errors.Is(preparationContext.Err(), context.Canceled) {
		test.Fatal("demand did not use fresh bounded admission and snapshot callbacks")
	}
	if !reflect.DeepEqual(fixture.identity.tokens, []string{fixture.token, fixture.token}) || !reflect.DeepEqual(fixture.identity.resources, []string{"https://mcp.example.test/mcp", "https://mcp.example.test/mcp"}) ||
		!reflect.DeepEqual(fixture.identity.snapshotTokens, []string{fixture.token}) || !reflect.DeepEqual(fixture.identity.snapshotResources, []string{"https://mcp.example.test/mcp"}) {
		test.Fatal("fresh authentication changed the bearer or resource audience")
	}
}

func TestConversationFindPreparationStrictBody(test *testing.T) {
	fixture := newConversationFindFixture(test)
	raw, err := json.Marshal(fixture.body)
	if err != nil {
		test.Fatal(err)
	}
	valid := string(raw)
	for _, candidate := range []struct{ name, body string }{
		{"empty", ""}, {"malformed", `{"version":`}, {"null", `null`}, {"array", `[]`},
		{"unknown field", strings.TrimSuffix(valid, "}") + `,"unexpected":true}`},
		{"unknown find field", strings.Replace(valid, `"find":{`, `"find":{"unexpected":true,`, 1)},
		{"wrong find type", strings.Replace(valid, `"query":"private 검색"`, `"query":12`, 1)},
		{"null find", strings.Replace(valid, `"find":{"query":"private 검색","cursor":"private-cursor"}`, `"find":null`, 1)},
		{"trailing object", valid + `{}`}, {"trailing null", valid + `null`}, {"trailing garbage", valid + `private-garbage`},
		{"version", strings.Replace(valid, `"version":1`, `"version":2`, 1)},
		{"repository", strings.Replace(valid, string(fixture.body.RepoID), "invalid", 1)},
		{"document hash", strings.Replace(valid, string(fixture.body.Hash), "invalid", 1)},
		{"empty query", strings.Replace(valid, "private 검색", "", 1)},
		{"long query", strings.Replace(valid, "private 검색", strings.Repeat("한", domain.MaxDocFindQueryRunes+1), 1)},
		{"long cursor", strings.Replace(valid, "private-cursor", strings.Repeat("x", base64.RawURLEncoding.EncodedLen(domain.MaxDocFindSelectorBytes)+1), 1)},
		{"unsupported identity", strings.Replace(valid, string(domain.DocumentIdentityRootV1), "future-identity", 1)},
		{"duplicate identity", strings.Replace(valid, `"document_identities":[`, `"document_identities":["`+string(domain.DocumentIdentityRootV1)+`",`, 1)},
		{"oversize payload", strings.Replace(valid, "private-cursor", strings.Repeat("x", domain.MaxConversationFindPreparationBytes), 1)},
		{"oversize whitespace", valid + strings.Repeat(" ", domain.MaxConversationFindPreparationBytes)},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			fixture.runtime.calls = nil
			handler := fixture.handler(test, func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) error {
				test.Fatal("invalid body reached demand")
				return nil
			})
			request := fixture.rawRequest(strings.NewReader(candidate.body))
			request.ContentLength = -1
			response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			handler.ServeHTTP(response, request)
			assertConversationFindResponse(test, response.ResponseRecorder, http.StatusUnprocessableEntity, "validation")
			if len(fixture.runtime.calls) != 1 || fixture.backend.lookups != 0 {
				test.Fatal("invalid body created repository scope or performed a lookup")
			}
		})
	}
	for _, cursor := range []string{"", strings.Repeat("x", base64.RawURLEncoding.EncodedLen(domain.MaxDocFindSelectorBytes))} {
		fixture.body.Find = domain.ConversationFindRequest{Query: strings.Repeat("한", domain.MaxDocFindQueryRunes), Cursor: cursor}
		handler := fixture.handler(test, func(_ context.Context, _, _ domain.ContentHash, request domain.ConversationFindRequest) error {
			if request != fixture.body.Find {
				test.Fatal("bounded query/cursor changed before application validation")
			}
			return nil
		})
		response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		handler.ServeHTTP(response, fixture.request(test))
		assertConversationFindResponse(test, response.ResponseRecorder, http.StatusAccepted, "")
	}
}

func TestConversationFindPreparationOnlyRequestedRoute(test *testing.T) {
	fixture := newConversationFindFixture(test)
	handler := fixture.handler(test, func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) error {
		test.Fatal("unexpected route reached demand")
		return nil
	})
	for _, path := range []string{"/mcp", "/", domain.MemoryReadPreparationPath, domain.ConversationFindPreparationPath + "/", domain.ConversationFindPreparationPath + "/extra"} {
		request := fixture.request(test)
		request.URL.Path = path
		response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		handler.ServeHTTP(response, request)
		assertConversationFindResponse(test, response.ResponseRecorder, http.StatusNotFound, "not_found")
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodOptions, http.MethodDelete} {
		request := fixture.request(test)
		request.Method = method
		response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost || response.Header().Get("Cache-Control") != "no-store" {
			test.Fatalf("unexpected method response: %d %s", response.Code, response.Body)
		}
	}
	if len(fixture.identity.tokens) != 0 || len(fixture.runtime.calls) != 0 {
		test.Fatal("unmounted route performed authentication or rate limiting")
	}
}

func TestConversationFindPreparationBodyByteBoundary(test *testing.T) {
	fixture := newConversationFindFixture(test)
	raw, err := json.Marshal(fixture.body)
	if err != nil {
		test.Fatal(err)
	}
	for _, size := range []int{domain.MaxConversationFindPreparationBytes, domain.MaxConversationFindPreparationBytes + 1} {
		calls := 0
		handler := fixture.handler(test, func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) error {
			calls++
			return nil
		})
		body := string(raw) + strings.Repeat(" ", size-len(raw))
		response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		handler.ServeHTTP(response, fixture.rawRequest(strings.NewReader(body)))
		if size == domain.MaxConversationFindPreparationBytes {
			assertConversationFindResponse(test, response.ResponseRecorder, http.StatusAccepted, "")
			if calls != 1 {
				test.Fatal("bounded body did not reach demand")
			}
		} else {
			assertConversationFindResponse(test, response.ResponseRecorder, http.StatusUnprocessableEntity, "validation")
			if calls != 0 {
				test.Fatal("oversize body reached demand")
			}
		}
	}
}
