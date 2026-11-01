package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryAdmissionHTTPBackend struct {
	*app.Service
	snapshots      inbound.MemorySnapshotQueries
	entered        chan struct{}
	scopes         atomic.Int64
	authorizations atomic.Int64
	reads          atomic.Int64
	readContext    context.Context
	scopeError     error
}

func (backend *memoryAdmissionHTTPBackend) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	backend.scopes.Add(1)
	if backend.entered != nil {
		close(backend.entered)
	}
	if authorize := inbound.DocumentReadAuthorizer(ctx); authorize != nil {
		ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
			backend.authorizations.Add(1)
			return authorize(bound, requested)
		})
	}
	backend.scopeError = backend.snapshots.WithMemoryReadResources(ctx, repo, read)
	return backend.scopeError
}

func (backend *memoryAdmissionHTTPBackend) GetMemoryDigest(ctx context.Context, repo, snapshot domain.ContentHash) (domain.MemoryDigest, error) {
	backend.reads.Add(1)
	backend.readContext = ctx
	return backend.snapshots.GetMemoryDigest(ctx, repo, snapshot)
}

func (backend *memoryAdmissionHTTPBackend) GetMemoryObject(ctx context.Context, repo, memory domain.ContentHash) (domain.MemoryDigest, error) {
	backend.reads.Add(1)
	backend.readContext = ctx
	return backend.snapshots.GetMemoryObject(ctx, repo, memory)
}

func (backend *memoryAdmissionHTTPBackend) GetMemoryProjection(ctx context.Context, repo, snapshot domain.ContentHash) (domain.MemoryProjection, error) {
	return backend.snapshots.GetMemoryProjection(ctx, repo, snapshot)
}

func (backend *memoryAdmissionHTTPBackend) QueryEffectiveMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	backend.reads.Add(1)
	backend.readContext = ctx
	return backend.Service.QueryEffectiveMemory(ctx, repo, request)
}

func (backend *memoryAdmissionHTTPBackend) QueryMemoryPositions(ctx context.Context, repo, snapshot domain.ContentHash, event string) (domain.MemoryPositions, error) {
	backend.reads.Add(1)
	backend.readContext = ctx
	return backend.Service.QueryMemoryPositions(ctx, repo, snapshot, event)
}

type memoryPositionQueryFunc func(context.Context, domain.ContentHash, domain.ContentHash, string) (domain.MemoryPositions, error)

func (query memoryPositionQueryFunc) QueryMemoryPositions(ctx context.Context, repo, snapshot domain.ContentHash, event string) (domain.MemoryPositions, error) {
	return query(ctx, repo, snapshot, event)
}

type memoryAdmissionHTTPStore interface {
	outbound.MetadataStore
	outbound.BlobStore
	outbound.RepositoryStore
}

type memoryAdmissionHTTPFixture struct {
	storage    memoryAdmissionHTTPStore
	identity   *app.IdentityService
	server     *Server
	backend    *memoryAdmissionHTTPBackend
	owner      domain.User
	viewer     domain.User
	session    domain.Session
	repository domain.Repository
	repo       domain.ContentHash
	snapshot   domain.ContentHash
	memory     domain.ContentHash
}

func newMemoryAdmissionHTTPFixture(test *testing.T) memoryAdmissionHTTPFixture {
	test.Helper()
	return newMemoryAdmissionHTTPFixtureWithStore(test, store.NewFSStore(test.TempDir()))
}

func newMemoryAdmissionHTTPFixtureWithStore(test *testing.T, storage memoryAdmissionHTTPStore) memoryAdmissionHTTPFixture {
	test.Helper()
	ctx := systemTestContext()
	identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
	suffix := domain.NewID("memory")
	owner, err := identity.Authenticate(ctx, "dev:"+suffix+"-owner@example.test:Owner")
	if err != nil {
		test.Fatal(err)
	}
	viewer, session, err := identity.Login(ctx, "dev:"+suffix+"-viewer@example.test:Viewer", "memory admission test")
	if err != nil {
		test.Fatal(err)
	}
	repository, err := identity.CreateRepository(ctx, owner, "MemoryAdmission")
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.AddMember(ctx, domain.Membership{RepositoryID: repository.ID, UserID: viewer.ID, Role: domain.RoleViewer}); err != nil {
		test.Fatal(err)
	}
	repo := domain.HashContent([]byte(repository.ID))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: repository.ID, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	document := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: suffix}}}}}}
	raw, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	document.Hash = domain.HashContent(raw)
	if _, err := storage.PutDoc(ctx, repo, document); err != nil {
		test.Fatal(err)
	}
	snapshot := document.Hash
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: snapshot, RepoID: repo, DocHash: snapshot, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC()}); err != nil {
		test.Fatal(err)
	}
	service := app.NewService(storage, storage, auth.NewTeamTokenAuth(), nil, storage)
	digest := domain.MemoryDigest{SnapshotID: snapshot, ClaimsVersion: domain.MemoryClaimsVersion,
		Fragments: []domain.MemoryFragment{{SourceSnapshot: snapshot, Claims: []domain.MemoryClaim{{Kind: "rationale", Text: "Private admitted memory"}}}}}
	memory, err := service.PutMemoryDigestCAS(ctx, repo, digest)
	if err != nil {
		test.Fatal(err)
	}
	backend := &memoryAdmissionHTTPBackend{Service: service, snapshots: memorySnapshotQueriesForTest(test, service)}
	server := NewServer(backend, identity)
	if err := server.SetMemorySnapshotQueries(backend); err != nil {
		test.Fatal(err)
	}
	server.SetEffectiveMemory(backend)
	server.SetMemoryPositions(backend)
	return memoryAdmissionHTTPFixture{storage, identity, server, backend, owner, viewer, session, repository, repo, snapshot, memory}
}

func (fixture memoryAdmissionHTTPFixture) request(ctx context.Context, route string) *http.Request {
	path := "/api/v1/repos/" + url.PathEscape(string(fixture.repo)) + "/"
	switch route {
	case "effective":
		query := url.Values{"snapshot_id": {string(fixture.snapshot)}, "memory_hash": {string(fixture.memory)}, "code_commit": {strings.Repeat("1", 40)}}
		path += "effective-memory?" + query.Encode()
	case "digest":
		path += "memories/" + url.PathEscape(string(fixture.snapshot))
	case "object":
		path += "memory-objects/" + url.PathEscape(string(fixture.memory))
	case "positions":
		path += "effective-memory/positions?snapshot_id=" + url.QueryEscape(string(fixture.snapshot))
	}
	request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+fixture.session.Token)
	return request
}

func memoryAdmissionAssertReleased(test *testing.T) {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
	if err != nil {
		test.Fatalf("memory response leaked shared bulk permit: %v", err)
	}
	release()
}

func TestMemoryReadHTTPRejectsSaturatedBulkBeforePayload(test *testing.T) {
	for _, route := range []string{"effective", "digest", "object", "positions"} {
		test.Run(route, func(test *testing.T) {
			fixture := newMemoryAdmissionHTTPFixture(test)
			_, release, err := app.AcquireResources(context.Background(), app.ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(release)
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), route))
			if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" ||
				!strings.Contains(response.Body.String(), "resource_overloaded") || fixture.backend.reads.Load() != 0 ||
				fixture.backend.authorizations.Load() != 0 || !errors.Is(fixture.backend.scopeError, domain.ErrResourceOverloaded) {
				test.Fatalf("saturated memory read crossed admission: %d %s reads=%d authorizations=%d err=%v", response.Code,
					response.Body, fixture.backend.reads.Load(), fixture.backend.authorizations.Load(), fixture.backend.scopeError)
			}
			release()
			memoryAdmissionAssertReleased(test)
		})
	}
}

func TestMemoryReadHTTPReauthorizesAfterWait(test *testing.T) {
	for _, route := range []string{"effective", "digest", "object", "positions"} {
		for _, change := range []string{"unchanged", "membership", "token", "expired_cookie", "public_expired_cookie"} {
			test.Run(route+"/"+change, func(test *testing.T) {
				fixture := newMemoryAdmissionHTTPFixture(test)
				ctx := systemTestContext()
				if change == "public_expired_cookie" {
					visibility := domain.VisibilityPublic
					if _, err := fixture.identity.UpdateRepositorySettings(ctx, fixture.owner.ID, fixture.repository.ID, app.RepositoryPatch{Visibility: &visibility}); err != nil {
						test.Fatal(err)
					}
				}
				fixture.backend.entered = make(chan struct{})
				_, release, err := app.AcquireResources(context.Background(), app.ResourceBulk)
				if err != nil {
					test.Fatal(err)
				}
				test.Cleanup(release)
				parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				test.Cleanup(cancel)
				request := fixture.request(parent, route)
				if strings.Contains(change, "cookie") {
					request.AddCookie(&http.Cookie{Name: sessionCookie, Value: fixture.session.Token})
					request.Header.Set("Authorization", "Bearer dev:"+fixture.owner.Email+":Owner")
				}
				response := &admissionDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
				done := make(chan struct{})
				go func() { defer close(done); fixture.server.Handler().ServeHTTP(response, request) }()
				test.Cleanup(func() {
					cancel()
					release()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						test.Error("memory handler did not exit")
					}
				})
				select {
				case <-fixture.backend.entered:
				case <-done:
					test.Fatalf("initial guard rejected legitimate viewer: %d %s", response.Code, response.Body)
				case <-parent.Done():
					test.Fatal("memory read never reached scope")
				}
				if fixture.backend.reads.Load() != 0 || fixture.backend.authorizations.Load() != 0 {
					test.Fatal("memory payload or current authorization ran before bulk admission")
				}
				want, code := http.StatusOK, ""
				switch change {
				case "membership":
					err = fixture.identity.RemoveMember(ctx, fixture.owner.ID, fixture.repository.ID, fixture.viewer.ID)
					want, code = http.StatusForbidden, "forbidden"
				case "token":
					err = fixture.identity.Logout(ctx, fixture.session.Token)
					want, code = http.StatusUnauthorized, "unauthenticated"
				case "expired_cookie", "public_expired_cookie":
					var stored domain.Session
					stored, err = fixture.storage.GetSession(ctx, domain.HashToken(fixture.session.Token))
					if err == nil {
						stored.CreatedAt, stored.ExpiresAt = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
						err = fixture.storage.CreateSession(ctx, stored)
					}
					if change == "expired_cookie" {
						want, code = http.StatusUnauthorized, "unauthenticated"
					}
				}
				if err != nil {
					test.Fatal(err)
				}
				release()
				select {
				case <-done:
				case <-parent.Done():
					test.Fatal("memory read did not complete after releasing shared bulk gate")
				}
				if response.Code != want || fixture.backend.authorizations.Load() != 1 {
					test.Fatalf("memory authorization drift: status=%d want=%d callbacks=%d body=%s", response.Code, want, fixture.backend.authorizations.Load(), response.Body)
				}
				deadline, _ := parent.Deadline()
				if !response.read.Equal(deadline) || !response.write.Equal(deadline) {
					test.Fatal("memory admission extended or lost original I/O deadline")
				}
				if want == http.StatusOK {
					if fixture.backend.reads.Load() != 1 || route != "positions" && !strings.Contains(response.Body.String(), "Private admitted memory") {
						test.Fatal("admitted memory read did not reuse scope for nested application call")
					}
					actor, system := inbound.RepositoryActor(fixture.backend.readContext)
					if system || change == "public_expired_cookie" && actor != "" || change == "unchanged" && actor != fixture.viewer.ID {
						test.Fatalf("memory read retained stale actor: %q system=%v", actor, system)
					}
				} else if fixture.backend.reads.Load() != 0 || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) || strings.Contains(response.Body.String(), "Private admitted memory") {
					test.Fatal("revoked queued request reached memory payload")
				}
				memoryAdmissionAssertReleased(test)
			})
		}
	}
}

func TestMemoryReadHTTPPermitCoversSerializationAndWriteFailure(test *testing.T) {
	for _, route := range []string{"effective", "digest", "object", "positions"} {
		for _, writeErr := range []error{nil, io.ErrClosedPipe} {
			test.Run(route+"/"+map[bool]string{true: "write_failure", false: "success"}[writeErr != nil], func(test *testing.T) {
				fixture := newMemoryAdmissionHTTPFixture(test)
				response := &admissionResponseWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: writeErr}
				fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), route))
				if response.Code != http.StatusOK || !response.checked || !errors.Is(response.err, context.DeadlineExceeded) || fixture.backend.reads.Load() != 1 || fixture.backend.authorizations.Load() != 1 {
					test.Fatalf("memory serialization escaped shared permit: status=%d checked=%v admission=%v body=%s", response.Code, response.checked, response.err, response.Body)
				}
				if writeErr == nil && !json.Valid(response.Body.Bytes()) {
					test.Fatal("admitted memory response is not valid JSON")
				}
				if !errors.Is(fixture.backend.readContext.Err(), context.Canceled) {
					test.Fatal("memory request deadline context leaked after response")
				}
				memoryAdmissionAssertReleased(test)
			})
		}
	}
}

func TestMemoryReadHTTPCancelledWaitDoesNotReadOrLeak(test *testing.T) {
	for _, route := range []string{"effective", "digest", "object", "positions"} {
		for _, mode := range []string{"cancel", "deadline"} {
			test.Run(route+"/"+mode, func(test *testing.T) {
				fixture := newMemoryAdmissionHTTPFixture(test)
				fixture.backend.entered = make(chan struct{})
				_, release, err := app.AcquireResources(context.Background(), app.ResourceBulk)
				if err != nil {
					test.Fatal(err)
				}
				test.Cleanup(release)
				ctx, cancel := context.WithCancel(context.Background())
				if mode == "deadline" {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				}
				test.Cleanup(cancel)
				response := httptest.NewRecorder()
				done := make(chan struct{})
				go func() { defer close(done); fixture.server.Handler().ServeHTTP(response, fixture.request(ctx, route)) }()
				select {
				case <-fixture.backend.entered:
				case <-time.After(time.Second):
					cancel()
					test.Fatal("memory request did not reach admission")
				}
				want := context.DeadlineExceeded
				if mode == "cancel" {
					cancel()
					want = context.Canceled
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					cancel()
					test.Fatal("cancelled memory request retained queue slot")
				}
				if !errors.Is(fixture.backend.scopeError, want) || response.Code == http.StatusOK || fixture.backend.reads.Load() != 0 || fixture.backend.authorizations.Load() != 0 {
					test.Fatalf("cancelled queued memory read ran: status=%d err=%v reads=%d", response.Code, fixture.backend.scopeError, fixture.backend.reads.Load())
				}
				release()
				memoryAdmissionAssertReleased(test)
			})
		}
	}
}

func TestMemoryReadHTTPUnsupportedReaderFailsClosedAndInitialGuardRemains(test *testing.T) {
	for _, route := range []string{"effective", "digest", "object", "positions"} {
		test.Run(route, func(test *testing.T) {
			fixture := newMemoryAdmissionHTTPFixture(test)
			fixture.server.b = struct{ Backend }{fixture.backend}
			fixture.server.memorySnapshots = nil
			fixture.server.effectiveMemory, fixture.server.memoryPositions = nil, nil
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), route))
			message := "Effective memory query unavailable"
			if route == "digest" || route == "object" {
				message = "Memory snapshot queries unavailable"
			}
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), message) || fixture.backend.reads.Load() != 0 {
				test.Fatalf("unsupported memory reader bypassed scope: %d %s", response.Code, response.Body)
			}
			if err := fixture.identity.RemoveMember(systemTestContext(), fixture.owner.ID, fixture.repository.ID, fixture.viewer.ID); err != nil {
				test.Fatal(err)
			}
			response = httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), route))
			if response.Code != http.StatusForbidden || fixture.backend.scopes.Load() != 0 || fixture.backend.reads.Load() != 0 {
				test.Fatalf("memory scope replaced initial guard: %d %s", response.Code, response.Body)
			}
		})
	}
}
