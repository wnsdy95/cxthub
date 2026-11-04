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

	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type preparedMemoryHTTPBackendTrap struct {
	Backend
	scopes  int
	queries int
}

func (backend *preparedMemoryHTTPBackendTrap) WithMemoryReadResources(context.Context, domain.ContentHash, func(context.Context) error) error {
	backend.scopes++
	return domain.ErrIntegrity
}

func (backend *preparedMemoryHTTPBackendTrap) QueryPreparedMemory(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
	backend.queries++
	return domain.MemoryReadPage{}, domain.ErrIntegrity
}

func TestSetPreparedMemoryRejectsNilWithoutReplacingReader(test *testing.T) {
	var typedNil *scopedPreparedMemoryQuery
	var typedNilOwner *memoryquery.PreparedQueries
	for _, installed := range []bool{false, true} {
		test.Run(map[bool]string{false: "empty", true: "installed"}[installed], func(test *testing.T) {
			server := &Server{}
			reader := &scopedPreparedMemoryQuery{
				MemoryReadScope: preparedMemoryScopeFunc(func(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
					return read(ctx)
				}),
				preparedMemoryQueryFunc: func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
					return domain.MemoryReadPage{}, nil
				},
			}
			if installed {
				if err := server.SetPreparedMemory(reader); err != nil {
					test.Fatal(err)
				}
			}
			for _, rejected := range []inbound.PreparedMemoryReader{nil, typedNil, typedNilOwner} {
				if err := server.SetPreparedMemory(rejected); !errors.Is(err, domain.ErrValidation) {
					test.Fatal("nil prepared reader accepted", err)
				}
				if installed && server.preparedMemory != reader || !installed && server.preparedMemory != nil {
					test.Fatal("rejected prepared reader replaced the previous state")
				}
			}
			replacement := &scopedPreparedMemoryQuery{reader.MemoryReadScope, reader.preparedMemoryQueryFunc}
			if err := server.SetPreparedMemory(replacement); err != nil || server.preparedMemory != replacement {
				test.Fatal("valid prepared reader could not be installed after rejection", err)
			}
		})
	}
}

func TestPreparedMemoryHTTPInjectedReaderOwnsScopeThroughResponse(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		queryErr error
		writeErr error
	}{
		{name: "ready"},
		{name: "pending", queryErr: &domain.MemoryReadPending{Cursor: "narrow-pending-floor"}},
		{name: "write_failure", writeErr: io.ErrClosedPipe},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newMemoryAdmissionHTTPFixture(test)
			broad := &preparedMemoryHTTPBackendTrap{Backend: fixture.backend}
			fixture.server.b = broad
			page := preparedMemoryHTTPPage(fixture)
			calls := 0
			var readContext context.Context
			reader := scopedPreparedMemoryQuery{fixture.backend, func(ctx context.Context, repo domain.ContentHash, input domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
				calls++
				readContext = ctx
				actor, system := inbound.RepositoryActor(ctx)
				if _, limited := ctx.Deadline(); !limited || actor != fixture.viewer.ID || system || repo != fixture.repo || input.Selection != page.Page.Selection {
					test.Fatal("injected prepared reader received an unbound request")
				}
				return page, scenario.queryErr
			}}
			if err := fixture.server.SetPreparedMemory(reader); err != nil {
				test.Fatal(err)
			}
			response := &admissionResponseWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: scenario.writeErr}
			fixture.server.Handler().ServeHTTP(response, preparedMemoryHTTPRequest(fixture, nil))
			if calls != 1 || fixture.backend.scopes.Load() != 1 || fixture.backend.authorizations.Load() != 1 ||
				broad.scopes != 0 || broad.queries != 0 || fixture.backend.reads.Load() != 0 {
				test.Fatal("prepared response discovered the broad backend or skipped the injected scope")
			}
			if !response.checked || !errors.Is(response.err, context.DeadlineExceeded) {
				test.Fatal("prepared response writing escaped the injected resource permit", response.err)
			}
			if readContext == nil || !errors.Is(readContext.Err(), context.Canceled) {
				test.Fatal("prepared request context outlived the response")
			}
			if scenario.queryErr != nil {
				assertPreparedMemoryHTTPError(test, response.ResponseRecorder, http.StatusServiceUnavailable, "memory_read_not_ready", "2", "narrow-pending-floor")
				if strings.Contains(response.Body.String(), "Private prepared memory") {
					test.Fatal("pending prepared read returned page contents")
				}
			} else {
				if response.Code != http.StatusOK {
					test.Fatal("injected prepared response failed", response.Code)
				}
				if scenario.writeErr == nil {
					var actual domain.MemoryReadPage
					if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil || !reflect.DeepEqual(actual, page) {
						test.Fatal("injected prepared response changed the wire page", err)
					}
				}
			}
			memoryAdmissionAssertReleased(test)
		})
	}
}
