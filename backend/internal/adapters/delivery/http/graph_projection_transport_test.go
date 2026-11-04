package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type graphProjectionHTTPBackend struct {
	Backend
	read func(context.Context, domain.ContentHash) error
	view domain.RepositoryView
}

func (backend *graphProjectionHTTPBackend) GetRepositoryView(ctx context.Context, repo domain.ContentHash) (domain.RepositoryView, error) {
	return backend.view, backend.read(ctx, repo)
}

func (backend *graphProjectionHTTPBackend) GetPendingView(ctx context.Context, repo domain.ContentHash) (domain.PendingView, error) {
	return domain.PendingView{ReadGeneration: backend.view.ReadGeneration, Graph: backend.view.Graph}, backend.read(ctx, repo)
}

func (backend *graphProjectionHTTPBackend) QueryGraphState(ctx context.Context, repo domain.ContentHash, _ string) (domain.GraphState, error) {
	return *backend.view.Graph, backend.read(ctx, repo)
}

func graphProjectionTransportView() domain.RepositoryView {
	generation := &domain.GraphReadGeneration{Generation: domain.HashContent([]byte("graph generation")), SourceEpoch: "12345678-abcd-4321-9876-123456789abc:2",
		AppliedEpoch: "12345678-abcd-4321-9876-123456789abc:1", Rules: 2, Complete: true, Current: false}
	return domain.RepositoryView{ReadGeneration: generation, Graph: &domain.GraphState{Version: 1, ReadGeneration: generation}}
}

func TestGraphProjectionHTTPRetryContract(t *testing.T) {
	for _, failure := range []struct {
		err   error
		code  string
		retry string
	}{
		{domain.ErrGraphProjectionNotReady, "graph_projection_pending", "1"},
		{fmt.Errorf("wrapped: %w", domain.ErrGraphProjectionNotReady), "graph_projection_pending", "1"},
		{domain.ErrGraphProjectionFailed, "graph_projection_failed", ""},
		{errors.Join(domain.ErrGraphProjectionFailed, domain.ErrGraphProjectionNotReady), "graph_projection_failed", ""},
	} {
		backend := &graphProjectionHTTPBackend{view: graphProjectionTransportView(), read: func(ctx context.Context, _ domain.ContentHash) error {
			if inbound.DocumentReadAuthorizer(ctx) == nil {
				t.Fatal("graph read is missing its current-rights callback")
			}
			return failure.err
		}}
		server := &Server{b: backend}
		if err := server.SetRepositoryGraphQueries(backend, inbound.RepositoryGraphCompatibility); err != nil {
			t.Fatal(err)
		}
		for _, handler := range []http.HandlerFunc{server.repositoryView, server.pendingView, server.graphState} {
			response := httptest.NewRecorder()
			handler(response, httptest.NewRequest(http.MethodGet, "/", nil))
			if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != failure.retry || !strings.Contains(response.Body.String(), `"code":"`+failure.code+`"`) {
				t.Fatalf("graph retry contract changed: %v %d %v %s", failure.err, response.Code, response.Header(), response.Body)
			}
		}
	}
}

func TestGraphProjectionHTTPGenerationMetadata(t *testing.T) {
	view := graphProjectionTransportView()
	backend := &graphProjectionHTTPBackend{view: view, read: func(context.Context, domain.ContentHash) error { return nil }}
	server := &Server{b: backend}
	if err := server.SetRepositoryGraphQueries(backend, inbound.RepositoryGraphCompatibility); err != nil {
		t.Fatal(err)
	}
	for _, encoding := range []string{"indexed-v1", "indexed-v2"} {
		for name, handler := range map[string]http.HandlerFunc{"view": server.repositoryView, "pending": server.pendingView, "graph": server.graphState} {
			response := httptest.NewRecorder()
			handler(response, httptest.NewRequest(http.MethodGet, "/?graph_encoding="+encoding, nil))
			if response.Code != http.StatusOK {
				t.Fatal(response.Code, response.Body)
			}
			var result struct {
				ReadGeneration *domain.GraphReadGeneration `json:"read_generation"`
				Graph          struct {
					ReadGeneration *domain.GraphReadGeneration `json:"read_generation"`
				} `json:"graph"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.ReadGeneration == nil || *result.ReadGeneration != *view.ReadGeneration ||
				(name != "graph" && (result.Graph.ReadGeneration == nil || *result.Graph.ReadGeneration != *result.ReadGeneration)) {
				t.Fatal("mixed or missing graph generation", name, encoding, response.Body)
			}
		}
	}
}

func TestGraphProjectionHTTPPostAdmissionAuthorization(t *testing.T) {
	for _, route := range []string{"/view", "/pending-view", "/graph-state"} {
		for _, change := range []string{"unchanged", "membership", "token", "foreign-repo"} {
			t.Run(route+"/"+change, func(t *testing.T) {
				ctx := systemTestContext()
				storage := store.NewFSStore(t.TempDir())
				identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
				owner, err := identity.Authenticate(ctx, "dev:graph-owner@example.test:Owner")
				if err != nil {
					t.Fatal(err)
				}
				reader, session, err := identity.Login(ctx, "dev:graph-reader@example.test:Reader", "graph read")
				if err != nil {
					t.Fatal(err)
				}
				record, err := identity.CreateRepository(ctx, owner, "graph-read")
				if err != nil {
					t.Fatal(err)
				}
				if err := storage.AddMember(ctx, domain.Membership{RepositoryID: record.ID, UserID: reader.ID, Role: domain.RoleViewer}); err != nil {
					t.Fatal(err)
				}
				repo := domain.HashContent([]byte(record.ID))
				if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID}); err != nil {
					t.Fatal(err)
				}
				backend := &graphProjectionHTTPBackend{Backend: app.NewService(storage, storage, nil, nil, storage), view: graphProjectionTransportView()}
				reads := 0
				backend.read = func(admitted context.Context, requestedRepo domain.ContentHash) error {
					authorize := inbound.DocumentReadAuthorizer(admitted)
					if authorize == nil {
						return errors.New("missing graph authorizer")
					}
					switch change {
					case "membership":
						err = identity.RemoveMember(ctx, owner.ID, record.ID, reader.ID)
					case "token":
						err = identity.Logout(ctx, session.Token)
					case "foreign-repo":
						requestedRepo = domain.HashContent([]byte("other repo"))
					}
					if err != nil {
						return err
					}
					if _, err := authorize(admitted, requestedRepo); err != nil {
						return err
					}
					reads++
					return nil
				}
				request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+string(repo)+route, nil)
				request.Header.Set("Authorization", "Bearer "+session.Token)
				response := httptest.NewRecorder()
				server := NewServer(backend, identity)
				if err := server.SetRepositoryGraphQueries(backend, inbound.RepositoryGraphCompatibility); err != nil {
					t.Fatal(err)
				}
				server.Handler().ServeHTTP(response, request)
				if change == "unchanged" {
					if response.Code != http.StatusOK || reads != 1 {
						t.Fatal(response.Code, reads, response.Body)
					}
				} else if (response.Code != http.StatusUnauthorized && response.Code != http.StatusForbidden) || reads != 0 {
					t.Fatal("stale actor admitted a graph read", response.Code, reads, response.Body)
				}
			})
		}
	}
}

func TestGraphProjectionRevisionHubPublishesWithoutCounterChanges(t *testing.T) {
	var current atomic.Pointer[domain.RepositoryChangeState]
	initial := domain.RepositoryChangeState{RepositoryRevision: domain.RepositoryRevision{Graph: 1, Pending: 1, Evidence: 1}, GraphProjection: &domain.GraphProjectionStatus{
		State: "catching_up", SourceEpoch: "12345678-abcd-4321-9876-123456789abc:2", AppliedEpoch: "12345678-abcd-4321-9876-123456789abc:1",
		Rules: 2, Generation: domain.HashContent([]byte("first generation")), Complete: true,
	}}
	current.Store(&initial)
	hub := repositoryChangeHub{}
	stream, release := hub.subscribe(domain.HashContent([]byte(t.Name())), func(context.Context, domain.ContentHash) (domain.RepositoryChangeState, error) {
		return *current.Load(), nil
	})
	defer release()
	for _, state := range []string{"catching_up", "ready", "needs_attention"} {
		next := initial
		projection := *initial.GraphProjection
		projection.State = state
		if state == "ready" {
			projection.Generation = domain.HashContent([]byte("second generation"))
			projection.AppliedEpoch, projection.Current = projection.SourceEpoch, true
		}
		next.GraphProjection = &projection
		current.Store(&next)
		select {
		case notice := <-stream:
			if notice.err != nil || !notice.revision.Equal(next) {
				t.Fatal("graph-only notice lost", notice)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("graph-only notice not published")
		}
	}
}

type graphAdmissionQueueWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
}

func (writer *graphAdmissionQueueWriter) SetWriteDeadline(time.Time) error {
	select {
	case <-writer.entered:
	default:
		close(writer.entered)
	}
	return nil
}

func TestGraphDirectHTTPQueuedAdmissionRechecksRevocation(t *testing.T) {
	for _, route := range []string{"view", "pending-view", "graph-state"} {
		t.Run(route, func(t *testing.T) {
			ctx := systemTestContext()
			storage := store.NewFSStore(t.TempDir())
			identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
			owner, err := identity.Authenticate(ctx, "dev:queued-graph-owner@example.test:Owner")
			if err != nil {
				t.Fatal(err)
			}
			const token = "dev:queued-graph-reader@example.test:Reader"
			reader, err := identity.Authenticate(ctx, token)
			if err != nil {
				t.Fatal(err)
			}
			record, err := identity.CreateRepository(ctx, owner, "queued-graph-read")
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.AddMember(ctx, domain.Membership{RepositoryID: record.ID, UserID: reader.ID, Role: domain.RoleViewer}); err != nil {
				t.Fatal(err)
			}
			repo := domain.HashContent([]byte(record.ID))
			if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID}); err != nil {
				t.Fatal(err)
			}
			var releases []func()
			for range 2 {
				_, release, err := app.AcquireResources(ctx, app.ResourceInteractive)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(release)
				releases = append(releases, release)
			}
			service := app.NewService(storage, storage, nil, nil, storage)
			queries, err := service.GraphQueriesForProfile(inbound.RepositoryGraphCompatibility)
			if err != nil {
				t.Fatal(err)
			}
			backend := &coldGraphBackend{Backend: service, queries: queries, attempts: make(chan coldGraphAttempt, 2)}
			server := NewServer(backend, identity)
			if err := server.SetRepositoryGraphQueries(backend, inbound.RepositoryGraphCompatibility); err != nil {
				t.Fatal(err)
			}
			handler := server.Handler()
			requestCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+string(repo)+"/"+route, nil).WithContext(requestCtx)
			request.Header.Set("Authorization", "Bearer "+token)
			writer := &graphAdmissionQueueWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{})}
			done := make(chan struct{})
			go func() { handler.ServeHTTP(writer, request); close(done) }()
			select {
			case <-writer.entered:
			case <-requestCtx.Done():
				t.Fatal("request did not reach post-guard admission")
			}
			if backend.calls.Load() != 0 {
				t.Fatal("graph read bypassed occupied admission")
			}
			if err := identity.RemoveMember(ctx, owner.ID, record.ID, reader.ID); err != nil {
				t.Fatal(err)
			}
			for _, release := range releases {
				release()
			}
			select {
			case <-done:
			case <-requestCtx.Done():
				t.Fatal("queued direct read did not finish")
			}
			if writer.Code != http.StatusForbidden || backend.calls.Load() != 1 {
				t.Fatal("queued direct graph read retained stale access", writer.Code, backend.calls.Load(), writer.Body)
			}
		})
	}
}
