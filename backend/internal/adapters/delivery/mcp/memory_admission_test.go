package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type memoryAdmissionBackend struct {
	*app.Service
	snapshots inbound.MemorySnapshotQueries
	entered   chan struct{}
	once      sync.Once
	reads     atomic.Int64
	repo      atomic.Pointer[domain.Repo]
	after     func(context.Context) error
	deadline  time.Time
}

func (backend *memoryAdmissionBackend) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	backend.deadline, _ = ctx.Deadline()
	backend.once.Do(func() { close(backend.entered) })
	return backend.snapshots.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		if err := read(bound); err != nil {
			return err
		}
		if backend.after != nil {
			return backend.after(bound)
		}
		return nil
	})
}

func (backend *memoryAdmissionBackend) ListRepos(ctx context.Context, team string) ([]domain.Repo, error) {
	if repo := backend.repo.Load(); repo != nil {
		return []domain.Repo{*repo}, nil
	}
	return backend.Service.ListRepos(ctx, team)
}

func (backend *memoryAdmissionBackend) GetSnapshot(ctx context.Context, repo, snapshot domain.ContentHash) (domain.Snapshot, error) {
	backend.reads.Add(1)
	return backend.Service.GetSnapshot(ctx, repo, snapshot)
}

type memoryAdmissionIdentity struct {
	IdentityBackend
	resolves atomic.Int64
	swapUser atomic.Bool
	mu       sync.Mutex
	tokens   []string
	resource []string
}

func (identity *memoryAdmissionIdentity) ResolveMCPUser(ctx context.Context, token string, resources ...string) (domain.User, error) {
	identity.resolves.Add(1)
	identity.mu.Lock()
	identity.tokens = append(identity.tokens, token)
	identity.resource = append(identity.resource, strings.Join(resources, ","))
	identity.mu.Unlock()
	user, err := identity.IdentityBackend.ResolveMCPUser(ctx, token, resources...)
	if err == nil && identity.swapUser.Load() {
		user.ID = "different-user"
	}
	return user, err
}

type memoryAdmissionFixture struct {
	storage    *store.FSStore
	identity   *app.IdentityService
	observed   *memoryAdmissionIdentity
	backend    *memoryAdmissionBackend
	server     *Server
	owner      domain.User
	reader     domain.User
	repository domain.Repository
	repo       domain.Repo
	snapshot   domain.Snapshot
	memory     domain.ContentHash
	token      string
}

func newMemoryAdmissionFixture(test *testing.T) *memoryAdmissionFixture {
	test.Helper()
	ctx := systemTestContext()
	storage := store.NewFSStore(test.TempDir())
	identity := app.NewIdentityService(nil, storage)
	owner := domain.User{ID: "memory-owner", Username: "memory-owner", Email: "owner@example.test", Name: "Owner"}
	reader := domain.User{ID: "memory-reader", Username: "memory-reader", Email: "reader@example.test", Name: "Reader"}
	for _, user := range []domain.User{owner, reader} {
		if err := storage.UpsertUser(ctx, user); err != nil {
			test.Fatal(err)
		}
	}
	repository, err := identity.CreateRepository(ctx, owner, "Memory-admission")
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
	snapshot := domain.Snapshot{ID: domain.HashContent([]byte("memory admission snapshot")), RepoID: repo.ID,
		DocHash: domain.HashContent([]byte("memory admission snapshot")), Branch: "main", CreatedAt: time.Unix(123, 0).UTC()}
	memory, err := storage.PutMemory(ctx, repo.ID, domain.MemoryDigest{SnapshotID: snapshot.ID,
		Summary: "Memory is unchanged: \ud55c\uae00", KeyFacts: []string{"historical fact"}, OpenTasks: []string{"retained task"}})
	if err != nil {
		test.Fatal(err)
	}
	snapshot.MemoryHash = memory
	if err := storage.PutSnapshot(ctx, snapshot); err != nil {
		test.Fatal(err)
	}
	if err := storage.CompareAndSwapRef(ctx, repo.ID, domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", Target: snapshot.ID}, ""); err != nil {
		test.Fatal(err)
	}
	pair, err := identity.IssueMCPTokenPair(ctx, reader.ID, "memory-admission-client", "https://memory.example.test/mcp")
	if err != nil {
		test.Fatal(err)
	}
	backend := &memoryAdmissionBackend{Service: app.NewService(storage, storage, nil, nil, storage), entered: make(chan struct{})}
	backend.snapshots = memorySnapshotQueriesForTest(test, backend.Service)
	observed := &memoryAdmissionIdentity{IdentityBackend: identity}
	server, err := NewServer(backend, observed, storage, "https://memory.example.test")
	if err != nil {
		test.Fatal(err)
	}
	setLiveMemoryFixture(test, server, backend.Service, backend)
	if err := server.SetMemorySnapshotQueries(memorySnapshotScopedTestReader{backend.snapshots, backend}); err != nil {
		test.Fatal(err)
	}
	return &memoryAdmissionFixture{storage: storage, identity: identity, observed: observed, backend: backend, server: server,
		owner: owner, reader: reader, repository: repository, repo: repo, snapshot: snapshot, memory: memory, token: pair.AccessToken}
}

func (fixture *memoryAdmissionFixture) request(ctx context.Context, args toolArgs) *http.Request {
	args.Repository = string(fixture.repo.ID)
	params, _ := json.Marshal(map[string]any{"name": "memory_load", "arguments": args})
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call", Params: params})
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body))).WithContext(ctx)
	for key, value := range remoteMCPHeaders(fixture.token) {
		request.Header.Set(key, value)
	}
	return request
}

func memoryAdmissionResult(test *testing.T, response *httptest.ResponseRecorder) (string, bool) {
	test.Helper()
	if response.Code != http.StatusOK {
		test.Fatalf("transport status=%d body=%s", response.Code, response.Body)
	}
	var envelope struct {
		Result struct {
			Error   bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || len(envelope.Result.Content) != 1 {
		test.Fatal("invalid tool result", err, response.Body)
	}
	return envelope.Result.Content[0].Text, envelope.Result.Error
}

func awaitMemoryAdmission(test *testing.T, entered <-chan struct{}) {
	test.Helper()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		test.Fatal("memory request did not reach admission")
	}
}

func holdMemoryBulk(test *testing.T) func() {
	test.Helper()
	_, release, err := app.AcquireResources(context.Background(), app.ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(release)
	return release
}

func TestMCPMemoryAdmissionQueuedRevocation(test *testing.T) {
	for _, change := range []string{"token", "role", "binding", "identity"} {
		test.Run(change, func(test *testing.T) {
			fixture := newMemoryAdmissionFixture(test)
			release := holdMemoryBulk(test)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			request := fixture.request(ctx, toolArgs{Mode: "stored", Ref: string(fixture.snapshot.ID)})
			go func() { fixture.server.Handler().ServeHTTP(response, request); close(done) }()
			awaitMemoryAdmission(test, fixture.backend.entered)
			if fixture.backend.reads.Load() != 0 || fixture.observed.resolves.Load() != 1 {
				test.Fatal("memory read passed occupied shared gate")
			}
			switch change {
			case "token":
				if err := fixture.identity.RevokeMCPToken(systemTestContext(), fixture.token, "memory-admission-client"); err != nil {
					test.Fatal(err)
				}
			case "role":
				if err := fixture.identity.RemoveMember(systemTestContext(), fixture.owner.ID, fixture.repository.ID, fixture.reader.ID); err != nil {
					test.Fatal(err)
				}
			case "binding":
				other, err := fixture.identity.CreateRepository(systemTestContext(), fixture.owner, "Private-replacement")
				if err != nil {
					test.Fatal(err)
				}
				fresh := fixture.repo
				fresh.RepositoryID = other.ID
				fixture.backend.repo.Store(&fresh)
			case "identity":
				fixture.observed.swapUser.Store(true)
			}
			release()
			awaitMemoryAdmission(test, done)
			text, failed := memoryAdmissionResult(test, response)
			if !failed || fixture.backend.reads.Load() != 0 || strings.Contains(text, "Memory is unchanged") {
				test.Fatal("stale authority reached memory", text, fixture.backend.reads.Load())
			}
			if fixture.observed.resolves.Load() != 2 {
				test.Fatal("bearer was not reauthenticated after admission", fixture.observed.resolves.Load())
			}
			for index, resource := range fixture.observed.resource {
				if resource != fixture.server.resource || fixture.observed.tokens[index] != fixture.token {
					test.Fatal("reauthentication lost captured token/audience")
				}
			}
		})
	}
}

func TestMCPMemoryAdmissionSaturationAndCancellation(test *testing.T) {
	for _, cancellation := range []string{"deadline", "cancel", "overloaded"} {
		test.Run(cancellation, func(test *testing.T) {
			fixture := newMemoryAdmissionFixture(test)
			release := holdMemoryBulk(test)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if cancellation == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			defer cancel()
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				fixture.server.Handler().ServeHTTP(response, fixture.request(ctx, toolArgs{Mode: "stored", Ref: string(fixture.snapshot.ID)}))
				close(done)
			}()
			awaitMemoryAdmission(test, fixture.backend.entered)
			_, interactiveRelease, err := app.AcquireResources(context.Background(), app.ResourceInteractive)
			if err != nil {
				test.Fatal("bulk waiter consumed interactive capacity", err)
			}
			interactiveRelease()
			if cancellation == "cancel" {
				cancel()
			}
			awaitMemoryAdmission(test, done)
			text, failed := memoryAdmissionResult(test, response)
			if !failed || fixture.backend.reads.Load() != 0 || fixture.observed.resolves.Load() != 1 {
				test.Fatal("saturated memory request bypassed admission", text)
			}
			if fixture.backend.deadline.IsZero() || time.Until(fixture.backend.deadline) > memoryRequestLimit {
				test.Fatal("memory request has no bounded deadline")
			}
			release()
			response = httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), toolArgs{Mode: "stored", Ref: string(fixture.snapshot.ID)}))
			if text, failed := memoryAdmissionResult(test, response); failed {
				test.Fatal("failed request leaked permit", text)
			}
		})
	}
}

func TestMCPMemoryAdmissionRefreshesRepositoryMetadataAfterQueue(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	ctx := systemTestContext()
	snapshot := fixture.snapshot
	snapshot.ID = domain.HashContent([]byte("new default branch snapshot"))
	snapshot.DocHash = snapshot.ID
	snapshot.Branch = "replacement"
	memory, err := fixture.storage.PutMemory(ctx, fixture.repo.ID, domain.MemoryDigest{SnapshotID: snapshot.ID, Summary: "Fresh default branch"})
	if err != nil {
		test.Fatal(err)
	}
	snapshot.MemoryHash = memory
	if err := fixture.storage.PutSnapshot(ctx, snapshot); err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.CompareAndSwapRef(ctx, fixture.repo.ID, domain.Ref{RepoID: fixture.repo.ID, Kind: domain.RefBranch, Name: snapshot.Branch, Target: snapshot.ID}, ""); err != nil {
		test.Fatal(err)
	}
	release := holdMemoryBulk(test)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), toolArgs{Mode: "stored"}))
		close(done)
	}()
	awaitMemoryAdmission(test, fixture.backend.entered)
	fresh := fixture.repo
	fresh.DefaultBranch = snapshot.Branch
	fixture.backend.repo.Store(&fresh)
	release()
	awaitMemoryAdmission(test, done)
	text, failed := memoryAdmissionResult(test, response)
	if failed || !strings.Contains(text, "Fresh default branch") || strings.Contains(text, "Memory is unchanged") {
		test.Fatal("queued memory read reused stale repository metadata", text)
	}
}

func TestMCPMemoryAdmissionModesPreserveSerializedResultsAndNestedScope(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	args := []toolArgs{
		{Ref: string(fixture.snapshot.ID)},
		{Mode: "project", Ref: "main"},
		{Mode: "stored", Ref: string(fixture.snapshot.ID)},
		{Mode: "stored", Ref: string(fixture.snapshot.ID), MemoryHash: string(fixture.memory)},
		{Ref: string(fixture.snapshot.ID), MemoryHash: string(fixture.memory)},
		{Mode: "effective", Ref: string(fixture.snapshot.ID), CodeCommit: strings.Repeat("a", 40)},
	}
	for _, args := range args {
		test.Run(args.Mode+args.Ref+args.MemoryHash, func(test *testing.T) {
			args.Repository = string(fixture.repo.ID)
			fixture.backend.after = nil
			want, err := fixture.server.memoryPage(systemTestContext(), fixture.repo, args)
			if err != nil {
				test.Fatal("baseline", err)
			}
			observed := false
			fixture.backend.after = func(bound context.Context) error {
				observed = true
				actor, system := inbound.RepositoryActor(bound)
				if actor != fixture.reader.ID || system {
					test.Fatal("memory scope elevated identity", actor, system)
				}
				before := fixture.observed.resolves.Load()
				if _, err := fixture.backend.Service.GetMemoryObject(bound, fixture.repo.ID, fixture.memory); err != nil {
					return err
				}
				if fixture.observed.resolves.Load() != before {
					test.Fatal("nested memory read repeated authorization")
				}
				probe, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				_, release, err := app.AcquireResources(probe, app.ResourceBulk)
				if release != nil {
					release()
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					test.Fatal("permit released before serialized page callback returned", err)
				}
				return nil
			}
			response := httptest.NewRecorder()
			before := fixture.observed.resolves.Load()
			fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), args))
			got, failed := memoryAdmissionResult(test, response)
			if failed || got != want || !observed || fixture.observed.resolves.Load() != before+2 {
				test.Fatal("scope changed memory response or authentication count", got, want, fixture.observed.resolves.Load()-before)
			}
		})
	}
}

type memoryDirectIdentity struct {
	IdentityBackend
	repository domain.Repository
	reader     string
}

func (identity memoryDirectIdentity) GetRepository(context.Context, string) (domain.Repository, error) {
	return identity.repository, nil
}

func (identity memoryDirectIdentity) RoleOf(_ context.Context, repository, user string) (domain.MemberRole, bool) {
	return domain.RoleViewer, repository == identity.repository.ID && user == identity.reader
}

func (memoryDirectIdentity) HasBreakGlassAccess(context.Context, string, string) (bool, error) {
	return false, nil
}

func TestMCPMemoryAdmissionDirectCallerAndUnsupportedBackend(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	fixture.server.identity = memoryDirectIdentity{repository: fixture.repository, reader: fixture.reader.ID}
	args, _ := json.Marshal(toolArgs{Repository: string(fixture.repo.ID), Mode: "stored", Ref: string(fixture.snapshot.ID)})
	result, err := fixture.server.runTool(context.Background(), fixture.reader, "memory_load", args)
	if err != nil || !strings.Contains(result, "Memory is unchanged") || fixture.observed.resolves.Load() != 0 {
		test.Fatal("direct caller did not use current user authorization", result, err)
	}
	fixture.server.context = struct{ ContextBackend }{fixture.backend}
	fixture.server.memorySnapshots = nil
	if result, err := fixture.server.runTool(context.Background(), fixture.reader, "memory_load", args); err == nil || result != "" {
		test.Fatal("unsupported memory scope did not fail closed", result, err)
	}
}

func TestMCPMemoryAdmissionProductionNeverFallsBackFromInvalidBearer(test *testing.T) {
	for _, token := range []string{"", "invalid", "wrong-audience"} {
		test.Run(token, func(test *testing.T) {
			fixture := newMemoryAdmissionFixture(test)
			if token == "wrong-audience" {
				pair, err := fixture.identity.IssueMCPTokenPair(systemTestContext(), fixture.reader.ID, "memory-admission-client", "https://other.example.test/mcp")
				if err != nil {
					test.Fatal(err)
				}
				token = pair.AccessToken
			}
			request := fixture.request(context.Background(), toolArgs{Mode: "stored", Ref: string(fixture.snapshot.ID)})
			request.Header.Set("Authorization", "Bearer "+token)
			request = request.WithContext(inbound.WithDocumentReadAuthorizer(request.Context(), fixture.server.memoryReadAuthorizer(fixture.reader, nil)))
			var call rpcRequest
			if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
				test.Fatal(err)
			}
			result, rpcErr := fixture.server.dispatch(request, fixture.reader, call)
			body, _ := json.Marshal(result)
			if rpcErr != nil || !strings.Contains(string(body), `"isError":true`) || fixture.backend.reads.Load() != 0 {
				test.Fatal("invalid bearer fell back to current-user callback", string(body), rpcErr)
			}
		})
	}
}
