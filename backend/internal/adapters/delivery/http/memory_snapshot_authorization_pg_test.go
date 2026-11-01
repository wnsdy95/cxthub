//go:build postgres

package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type snapshotMemoryHTTPBackend struct {
	*app.Service
	snapshots inbound.MemorySnapshotQueries
	storage   *store.PostgresStore
	before    func(context.Context) error
	actor     string
	reads     int
	checks    int
}

type qualifiedMemoryHTTPBackend struct {
	*app.Service
	qualification domain.ContentHash
	before        func() error
}

func (backend *qualifiedMemoryHTTPBackend) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return backend.Service.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		if backend.before != nil {
			if err := backend.before(); err != nil {
				return err
			}
		}
		return read(bound)
	})
}

func (backend *qualifiedMemoryHTTPBackend) QueryEffectiveMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	return backend.Service.QueryQualifiedMemory(ctx, repo, backend.qualification, request)
}

func TestPGMemoryHTTPQualifiedResultUsesTransportAuthorization(test *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("requires disposable CXT_TEST_DSN")
	}
	ctx, cancel := context.WithTimeout(systemTestContext(), 40*time.Second)
	defer cancel()
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer storage.Close()
	if _, err := storage.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		test.Fatal(err)
	}
	fixture := newMemoryAdmissionHTTPFixtureWithStore(test, storage)
	selection := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: fixture.snapshot, MemoryHash: fixture.memory, CodeCommit: strings.Repeat("1", 40)}}
	prepared, err := fixture.backend.Service.PrepareMemoryRead(ctx, fixture.repo, selection, nil)
	if err != nil {
		test.Fatal("prepare", err)
	}
	identifier, err := fixture.backend.Service.PublishPreparedMemoryRead(ctx, prepared, time.Now().Add(time.Hour))
	if err != nil {
		test.Fatal("qualify", err)
	}
	backend := &qualifiedMemoryHTTPBackend{Service: fixture.backend.Service, qualification: identifier}
	server := NewServer(backend, fixture.identity)
	server.SetEffectiveMemory(backend)
	for _, scenario := range []string{"session", "raw IDP", "revoked after admission"} {
		test.Run(scenario, func(test *testing.T) {
			request := fixture.request(ctx, "effective")
			if scenario == "raw IDP" {
				request.Header.Set("Authorization", "Bearer dev:"+fixture.viewer.Email)
			}
			want := http.StatusOK
			if scenario == "revoked after admission" {
				want = http.StatusUnauthorized
				backend.before = func() error { return fixture.identity.Logout(context.Background(), fixture.session.Token) }
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != want || want == http.StatusOK && !strings.Contains(response.Body.String(), "Private admitted memory") {
				test.Fatalf("qualified HTTP status=%d want=%d body=%s", response.Code, want, response.Body)
			}
		})
	}
}

func (backend *snapshotMemoryHTTPBackend) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return backend.snapshots.WithMemoryReadResources(ctx, repo, func(admitted context.Context) error {
		if backend.before != nil {
			if err := backend.before(admitted); err != nil {
				return err
			}
		}
		return backend.storage.WithinReadSnapshot(admitted, func(bound context.Context) error {
			authorize := inbound.DocumentSnapshotReadAuthorizer(bound)
			if authorize == nil {
				return errors.New("actual HTTP handler did not supply a snapshot authorizer")
			}
			if _, err := authorize(bound, domain.HashContent([]byte("wrong repo"))); !errors.Is(err, domain.ErrForbidden) {
				return errors.New("snapshot authorizer accepted another repository")
			}
			current, err := authorize(inbound.WithRepositoryActor(bound, "forged actor"), repo)
			backend.checks++
			if err != nil {
				return err
			}
			actor, system := inbound.RepositoryActor(current)
			if actor != backend.actor || system || !backend.storage.InReadOnlyTransaction(current) {
				return errors.New("snapshot authorizer detached context or trusted inherited actor")
			}
			return read(current)
		})
	})
}

func (backend *snapshotMemoryHTTPBackend) GetMemoryDigest(ctx context.Context, repo, snapshot domain.ContentHash) (domain.MemoryDigest, error) {
	backend.reads++
	if !backend.storage.InReadOnlyTransaction(ctx) {
		return domain.MemoryDigest{}, errors.New("not a snapshot-bound read")
	}
	return backend.snapshots.GetMemoryDigest(ctx, repo, snapshot)
}

func (backend *snapshotMemoryHTTPBackend) GetMemoryObject(ctx context.Context, repo, memory domain.ContentHash) (domain.MemoryDigest, error) {
	return backend.snapshots.GetMemoryObject(ctx, repo, memory)
}

func (backend *snapshotMemoryHTTPBackend) GetMemoryProjection(ctx context.Context, repo, snapshot domain.ContentHash) (domain.MemoryProjection, error) {
	return backend.snapshots.GetMemoryProjection(ctx, repo, snapshot)
}

func TestPGMemoryHTTPSnapshotAuthorization(test *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("requires disposable CXT_TEST_DSN")
	}
	ctx, cancel := context.WithTimeout(systemTestContext(), 40*time.Second)
	defer cancel()
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer storage.Close()
	if _, err := storage.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		test.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	connection, err := pgxpool.New(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Close()
	revoker := app.NewIdentityService(auth.NewDevVerifier(), peer)
	for _, change := range []string{"unchanged", "raw IDP", "legacy", "revoked session", "revoked membership", "public anonymous", "public revoked cookie", "public privatized", "rebound repository", "expired during admission"} {
		test.Run(change, func(test *testing.T) {
			fixture := newMemoryAdmissionHTTPFixtureWithStore(test, storage)
			backend := &snapshotMemoryHTTPBackend{Service: fixture.backend.Service, snapshots: memorySnapshotQueriesForTest(test, fixture.backend.Service), storage: storage, actor: fixture.viewer.ID}
			server := NewServer(backend, fixture.identity)
			if err := server.SetMemorySnapshotQueries(backend); err != nil {
				test.Fatal(err)
			}
			request := fixture.request(ctx, "digest")
			want := http.StatusOK
			if change == "raw IDP" {
				request.Header.Set("Authorization", "Bearer dev:"+fixture.viewer.Email)
			}
			if change == "legacy" {
				legacy := domain.Session{Token: domain.NewID("sess_"), UserID: fixture.viewer.ID, ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now().UTC()}
				if err := storage.CreateSession(ctx, legacy); err != nil {
					test.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+legacy.Token)
				backend.before = func(context.Context) error {
					if _, err := peer.GetSession(ctx, legacy.Token); !errors.Is(err, domain.ErrNotFound) {
						return errors.New("legacy migration not completed during admission")
					}
					_, err := peer.GetSession(ctx, domain.HashToken(legacy.Token))
					return err
				}
			}
			switch change {
			case "revoked session":
				want = http.StatusUnauthorized
				backend.before = func(context.Context) error { return revoker.Logout(ctx, fixture.session.Token) }
			case "revoked membership":
				want = http.StatusForbidden
				backend.before = func(context.Context) error {
					return revoker.RemoveMember(ctx, fixture.owner.ID, fixture.repository.ID, fixture.viewer.ID)
				}
			case "public anonymous", "public privatized", "public revoked cookie":
				visibility := domain.VisibilityPublic
				if _, err := fixture.identity.UpdateRepositorySettings(ctx, fixture.owner.ID, fixture.repository.ID, app.RepositoryPatch{Visibility: &visibility}); err != nil {
					test.Fatal(err)
				}
				request.Header.Del("Authorization")
				backend.actor = ""
				if change == "public revoked cookie" {
					request.AddCookie(&http.Cookie{Name: sessionCookie, Value: fixture.session.Token})
					backend.before = func(context.Context) error { return revoker.Logout(ctx, fixture.session.Token) }
				}
				if change == "public privatized" {
					want = http.StatusUnauthorized
					backend.before = func(context.Context) error {
						visibility = domain.VisibilityPrivate
						_, err := revoker.UpdateRepositorySettings(ctx, fixture.owner.ID, fixture.repository.ID, app.RepositoryPatch{Visibility: &visibility})
						return err
					}
				}
			case "rebound repository":
				want = http.StatusForbidden
				backend.before = func(context.Context) error {
					record, err := fixture.identity.CreateRepository(ctx, fixture.owner, domain.NewID("other"))
					if err != nil {
						return err
					}
					_, err = connection.Exec(ctx, `UPDATE repos SET repository_id=$2 WHERE id=$1`, fixture.repo, record.ID)
					return err
				}
			case "expired during admission":
				want = http.StatusUnauthorized
				backend.before = func(context.Context) error {
					_, err := connection.Exec(ctx, `UPDATE sessions SET expires_at=$2 WHERE token=$1`, domain.HashToken(fixture.session.Token), time.Now().Add(-time.Second))
					return err
				}
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != want || backend.checks != 1 || (want == http.StatusOK) != (backend.reads == 1) {
				test.Fatalf("status=%d want=%d checks=%d reads=%d body=%s", response.Code, want, backend.checks, backend.reads, response.Body)
			}
		})
	}
}
