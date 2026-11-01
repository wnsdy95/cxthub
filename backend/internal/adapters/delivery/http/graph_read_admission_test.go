package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type coldGraphAttempt struct {
	err      error
	deadline time.Time
}

type coldGraphBackend struct {
	Backend
	queries  inbound.RepositoryGraphQueries
	attempts chan coldGraphAttempt
	calls    atomic.Int64
}

func (backend *coldGraphBackend) report(ctx context.Context, err error) {
	deadline, _ := ctx.Deadline()
	backend.calls.Add(1)
	backend.attempts <- coldGraphAttempt{err, deadline}
}

func (backend *coldGraphBackend) QueryGraphState(ctx context.Context, repo domain.ContentHash, position string) (domain.GraphState, error) {
	view, err := backend.queries.QueryGraphState(ctx, repo, position)
	backend.report(ctx, err)
	return view, err
}

func (backend *coldGraphBackend) GetRepositoryView(ctx context.Context, repo domain.ContentHash) (domain.RepositoryView, error) {
	view, err := backend.queries.GetRepositoryView(ctx, repo)
	backend.report(ctx, err)
	return view, err
}

func (backend *coldGraphBackend) GetPendingView(ctx context.Context, repo domain.ContentHash) (domain.PendingView, error) {
	view, err := backend.queries.GetPendingView(ctx, repo)
	backend.report(ctx, err)
	return view, err
}

func TestColdGraphHTTPReopensWithCurrentAccessAndOriginalDeadline(t *testing.T) {
	for _, route := range []string{"view", "graph-state", "pending-view"} {
		for _, revoke := range []bool{false, true} {
			t.Run(route+map[bool]string{false: "/success", true: "/revoked"}[revoke], func(t *testing.T) {
				ctx := systemTestContext()
				storage := store.NewFSStore(t.TempDir())
				identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
				owner, err := identity.Authenticate(ctx, "dev:cold-owner@example.test:Owner")
				if err != nil {
					t.Fatal(err)
				}
				const token = "dev:cold-viewer@example.test:Viewer"
				viewer, err := identity.Authenticate(ctx, token)
				if err != nil {
					t.Fatal(err)
				}
				record, err := identity.CreateRepository(ctx, owner, "cold-admission")
				if err != nil {
					t.Fatal(err)
				}
				if err := storage.AddMember(ctx, domain.Membership{RepositoryID: record.ID, UserID: viewer.ID, Role: domain.RoleViewer}); err != nil {
					t.Fatal(err)
				}
				repo := domain.HashContent([]byte(record.ID))
				if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID}); err != nil {
					t.Fatal(err)
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
				_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				requestCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+string(repo)+"/"+route+"?graph_encoding=indexed-v2", nil).WithContext(requestCtx)
				request.Header.Set("Authorization", "Bearer "+token)
				writer := &admissionResponseWriter{ResponseRecorder: httptest.NewRecorder()}
				done := make(chan struct{})
				go func() { handler.ServeHTTP(writer, request); close(done) }()
				var first coldGraphAttempt
				select {
				case first = <-backend.attempts:
				case <-requestCtx.Done():
					t.Fatal("first attempt did not return", requestCtx.Err())
				}
				if !errors.Is(first.err, domain.ErrResourceOverloaded) {
					t.Fatalf("cold attempt bypassed worker: %v", first.err)
				}
				if revoke {
					if err := identity.RemoveMember(ctx, owner.ID, record.ID, viewer.ID); err != nil {
						t.Fatal(err)
					}
				}
				release()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("handler did not finish")
				}
				if revoke {
					if writer.Code != http.StatusForbidden || backend.calls.Load() != 1 {
						t.Fatalf("revoked reader retried: status=%d calls=%d body=%s", writer.Code, backend.calls.Load(), writer.Body)
					}
				} else {
					if writer.Code != http.StatusOK || backend.calls.Load() != 2 || !errors.Is(writer.err, context.DeadlineExceeded) {
						t.Fatalf("retry/response admission: status=%d calls=%d write=%v body=%s", writer.Code, backend.calls.Load(), writer.err, writer.Body)
					}
					second := <-backend.attempts
					deadline, _ := requestCtx.Deadline()
					if first.deadline != deadline || second.deadline != deadline || second.err != nil {
						t.Fatalf("retry changed deadline/result: %+v %+v", first, second)
					}
				}
			})
		}
	}
}

func TestColdGraphHTTPExpiredCredentialsFollowPublicGuard(t *testing.T) {
	for _, public := range []bool{false, true} {
		t.Run(map[bool]string{false: "private revoked token", true: "public expired cookie"}[public], func(t *testing.T) {
			ctx := systemTestContext()
			storage := store.NewFSStore(t.TempDir())
			identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
			owner, session, err := identity.Login(ctx, "dev:cold-auth@example.test:Owner", "test")
			if err != nil {
				t.Fatal(err)
			}
			record, err := identity.CreateRepository(ctx, owner, "cold-auth")
			if err != nil {
				t.Fatal(err)
			}
			if public {
				visibility := domain.VisibilityPublic
				if _, err := identity.UpdateRepositorySettings(ctx, owner.ID, record.ID, app.RepositoryPatch{Visibility: &visibility}); err != nil {
					t.Fatal(err)
				}
				if err := identity.Logout(ctx, session.Token); err != nil {
					t.Fatal(err)
				}
			}
			repo := domain.HashContent([]byte(record.ID))
			if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID}); err != nil {
				t.Fatal(err)
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
			_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			requestCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+string(repo)+"/graph-state", nil).WithContext(requestCtx)
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
			writer := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { handler.ServeHTTP(writer, request); close(done) }()
			select {
			case attempt := <-backend.attempts:
				if !errors.Is(attempt.err, domain.ErrResourceOverloaded) {
					t.Fatal(attempt.err)
				}
			case <-requestCtx.Done():
				t.Fatal("missing first attempt")
			}
			if !public {
				if err := identity.Logout(ctx, session.Token); err != nil {
					t.Fatal(err)
				}
			}
			release()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("handler did not finish")
			}
			expected, calls := http.StatusUnauthorized, int64(1)
			if public {
				expected, calls = http.StatusOK, 2
			}
			if writer.Code != expected || backend.calls.Load() != calls {
				t.Fatalf("guard changed on retry: status=%d calls=%d body=%s", writer.Code, backend.calls.Load(), writer.Body)
			}
		})
	}
}
