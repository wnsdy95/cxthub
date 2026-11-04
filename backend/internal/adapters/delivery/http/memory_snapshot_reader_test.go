package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func memorySnapshotQueriesForTest(test *testing.T, service *app.Service) inbound.MemorySnapshotQueries {
	test.Helper()
	reader, err := service.MemorySnapshotQueries()
	if err != nil {
		test.Fatal(err)
	}
	return reader
}

type memorySnapshotHTTPReader struct {
	inbound.MemorySnapshotQueries
	scope inbound.MemoryReadScope
	read  func(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error)
}

func (reader *memorySnapshotHTTPReader) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return reader.scope.WithMemoryReadResources(ctx, repo, read)
}

func (reader *memorySnapshotHTTPReader) GetMemoryDigest(ctx context.Context, repo, snapshot domain.ContentHash) (domain.MemoryDigest, error) {
	return reader.read(ctx, repo, snapshot)
}

func (reader *memorySnapshotHTTPReader) GetMemoryObject(ctx context.Context, repo, memory domain.ContentHash) (domain.MemoryDigest, error) {
	return reader.read(ctx, repo, memory)
}

type memorySnapshotHTTPBackendTrap struct {
	Backend
	scopes int
	reads  int
}

func (backend *memorySnapshotHTTPBackendTrap) WithMemoryReadResources(context.Context, domain.ContentHash, func(context.Context) error) error {
	backend.scopes++
	return domain.ErrIntegrity
}

func (backend *memorySnapshotHTTPBackendTrap) GetMemoryDigest(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error) {
	backend.reads++
	return domain.MemoryDigest{}, domain.ErrIntegrity
}

func (backend *memorySnapshotHTTPBackendTrap) GetMemoryObject(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error) {
	backend.reads++
	return domain.MemoryDigest{}, domain.ErrIntegrity
}

func TestSetMemorySnapshotQueriesRejectsNilWithoutReplacingReader(test *testing.T) {
	var typedNil *memorySnapshotHTTPReader
	var typedNilOwner *memoryquery.SnapshotQueries
	for _, installed := range []bool{false, true} {
		test.Run(map[bool]string{false: "empty", true: "installed"}[installed], func(test *testing.T) {
			server := &Server{}
			reader := &memorySnapshotHTTPReader{}
			if installed {
				if err := server.SetMemorySnapshotQueries(reader); err != nil {
					test.Fatal(err)
				}
			}
			for _, rejected := range []inbound.MemorySnapshotQueries{nil, typedNil, typedNilOwner} {
				if err := server.SetMemorySnapshotQueries(rejected); !errors.Is(err, domain.ErrValidation) {
					test.Fatal("nil snapshot reader accepted", err)
				}
				if installed && server.memorySnapshots != reader || !installed && server.memorySnapshots != nil {
					test.Fatal("rejected reader replaced the previous state")
				}
			}
			replacement := &memorySnapshotHTTPReader{}
			if err := server.SetMemorySnapshotQueries(replacement); err != nil || server.memorySnapshots != replacement {
				test.Fatal("valid replacement rejected", err)
			}
		})
	}
}

func TestMemorySnapshotHTTPInjectedReaderOwnsScopeThroughResponse(test *testing.T) {
	for _, route := range []string{"digest", "object"} {
		for _, broadScope := range []bool{false, true} {
			for _, writeErr := range []error{nil, io.ErrClosedPipe} {
				test.Run(route+"/"+map[bool]string{false: "no_broad_scope", true: "poisoned_broad_scope"}[broadScope]+"/"+map[bool]string{false: "ready", true: "write_failure"}[writeErr != nil], func(test *testing.T) {
					fixture := newMemoryAdmissionHTTPFixture(test)
					broad := &memorySnapshotHTTPBackendTrap{Backend: fixture.backend}
					fixture.server.b = struct{ Backend }{fixture.backend}
					if broadScope {
						fixture.server.b = broad
					}
					want, err := fixture.backend.snapshots.GetMemoryObject(systemTestContext(), fixture.repo, fixture.memory)
					if err != nil {
						test.Fatal(err)
					}
					calls := 0
					var readContext context.Context
					reader := &memorySnapshotHTTPReader{scope: fixture.backend, read: func(ctx context.Context, repo, selected domain.ContentHash) (domain.MemoryDigest, error) {
						calls++
						readContext = ctx
						actor, system := inbound.RepositoryActor(ctx)
						identity := fixture.snapshot
						if route == "object" {
							identity = fixture.memory
						}
						if _, limited := ctx.Deadline(); !limited || actor != fixture.viewer.ID || system || repo != fixture.repo || selected != identity || inbound.DocumentReadAuthorizer(ctx) == nil {
							test.Fatal("snapshot reader received an unbound request")
						}
						return want, nil
					}}
					if err := fixture.server.SetMemorySnapshotQueries(reader); err != nil {
						test.Fatal(err)
					}
					response := &admissionResponseWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: writeErr}
					fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), route))
					if response.Code != http.StatusOK || calls != 1 || fixture.backend.scopes.Load() != 1 || fixture.backend.authorizations.Load() != 1 || broad.scopes != 0 || broad.reads != 0 || fixture.backend.reads.Load() != 0 {
						test.Fatal("snapshot response discovered broad capabilities or skipped admission", response.Code, response.Body)
					}
					if !response.checked || !errors.Is(response.err, context.DeadlineExceeded) || readContext == nil || !errors.Is(readContext.Err(), context.Canceled) {
						test.Fatal("snapshot response escaped its resource/deadline lifetime")
					}
					if writeErr == nil {
						var actual domain.MemoryDigest
						if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil || !reflect.DeepEqual(actual, want) {
							test.Fatal("snapshot response changed wire bytes", err)
						}
					}
					memoryAdmissionAssertReleased(test)
				})
			}
		}
	}
}

func TestMemorySnapshotHTTPMissingReaderDoesNotDiscoverBackend(test *testing.T) {
	for _, route := range []string{"digest", "object"} {
		test.Run(route, func(test *testing.T) {
			fixture := newMemoryAdmissionHTTPFixture(test)
			broad := &memorySnapshotHTTPBackendTrap{Backend: fixture.backend}
			server := NewServer(broad, fixture.identity)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, fixture.request(context.Background(), route))
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "Memory snapshot queries unavailable") || broad.scopes != 0 || broad.reads != 0 || fixture.backend.scopes.Load() != 0 {
				test.Fatal("missing snapshot reader rediscovered broad capabilities", response.Code, response.Body)
			}
		})
	}
}

func TestMemorySnapshotHTTPInjectedScopeAndQueryErrors(test *testing.T) {
	for _, route := range []string{"digest", "object"} {
		for _, scopeFailure := range []bool{false, true} {
			for _, failure := range []struct {
				name   string
				err    error
				status int
			}{
				{"missing", domain.ErrNotFound, http.StatusNotFound},
				{"corrupt", domain.ErrIntegrity, http.StatusUnprocessableEntity},
				{"forbidden", domain.ErrForbidden, http.StatusForbidden},
				{"unauthorized", domain.ErrUnauthorized, http.StatusUnauthorized},
			} {
				test.Run(route+"/"+map[bool]string{false: "query", true: "scope"}[scopeFailure]+"/"+failure.name, func(test *testing.T) {
					fixture := newMemoryAdmissionHTTPFixture(test)
					calls := 0
					reader := &memorySnapshotHTTPReader{scope: preparedMemoryScopeFunc(func(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
						if scopeFailure {
							return failure.err
						}
						return read(ctx)
					}), read: func(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error) {
						calls++
						return domain.MemoryDigest{Summary: "must not leak"}, failure.err
					}}
					if err := fixture.server.SetMemorySnapshotQueries(reader); err != nil {
						test.Fatal(err)
					}
					response := httptest.NewRecorder()
					fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), route))
					if response.Code != failure.status || calls != map[bool]int{false: 1, true: 0}[scopeFailure] || strings.Contains(response.Body.String(), "must not leak") || fixture.backend.scopes.Load() != 0 || fixture.backend.reads.Load() != 0 {
						test.Fatal("snapshot error mapping changed or rejected scope read payload", response.Code, response.Body)
					}
				})
			}
		}
	}
}
