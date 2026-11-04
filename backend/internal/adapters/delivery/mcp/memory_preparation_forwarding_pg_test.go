//go:build postgres

package mcp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/preparation"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/memorypreparation"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryPreparationProbe struct {
	outbound.MemoryPreparationRequester
	calls int
	fail  bool
}

type memoryPreparationBufferedResponse struct {
	*httptest.ResponseRecorder
	underlying http.ResponseWriter
}

func (response memoryPreparationBufferedResponse) Unwrap() http.ResponseWriter {
	return response.underlying
}

func (probe *memoryPreparationProbe) RequestMemoryPreparation(ctx context.Context, token string, request domain.MemoryReadPreparationRequest) error {
	probe.calls++
	if probe.fail {
		return domain.ErrMemoryPreparationUnavailable
	}
	return probe.MemoryPreparationRequester.RequestMemoryPreparation(ctx, token, request)
}

func TestPGMemoryPreparationForwardingColdMCPCompletes(test *testing.T) {
	for _, scenario := range []string{"ready", "outage", "acknowledgment_lost", "revoked"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newEffectiveMemorySourceTransportPG(test, false)
			repo := fixture.repo.ID
			identity := app.NewIdentityService(auth.NewDevVerifier(), fixture.storage)
			reader := app.NewService(fixture.storage, fixture.storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(fixture.storage), fixture.storage)
			for _, service := range []*app.Service{reader, fixture.service} {
				if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{repo}); err != nil {
					test.Fatal(err)
				}
			}
			if err := fixture.service.ConfigureMemoryReadPreparations(fixture.ctx, []domain.ContentHash{repo}); err != nil {
				test.Fatal(err)
			}
			if err := fixture.service.ConfigurePreparedMemoryDemands([]domain.ContentHash{repo}); err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() {
				if err := fixture.storage.ConfigureMemoryReadWork(fixture.ctx, repo, false); err != nil {
					test.Error(err)
				}
			})
			const origin = "https://memory-source.example.test"
			preparer := fixture.preparedReader(test, fixture.service)
			handler, err := preparation.NewMemoryPreparationHandler(fixture.service, identity, preparer, fixture.storage, origin, origin)
			if err != nil {
				test.Fatal(err)
			}
			var received atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				attempt := received.Add(1)
				if scenario == "revoked" {
					if err := identity.RevokeMCPToken(request.Context(), fixture.mcpToken, "memory-source-client"); err != nil {
						test.Error(err)
					}
				}
				if scenario == "acknowledgment_lost" && attempt == 1 {
					buffered := memoryPreparationBufferedResponse{ResponseRecorder: httptest.NewRecorder(), underlying: writer}
					handler.ServeHTTP(buffered, request)
					if buffered.Code != http.StatusAccepted {
						test.Error("fault injection did not follow durable acceptance", buffered.Code)
					}
					writer.Header().Set("Content-Type", "application/json")
					writer.WriteHeader(http.StatusServiceUnavailable)
					_, _ = writer.Write([]byte(`{"error":{"code":"memory_preparation_unavailable"}}`))
					return
				}
				handler.ServeHTTP(writer, request)
			}))
			defer api.Close()
			client, err := memorypreparation.NewClient(api.URL)
			if err != nil {
				test.Fatal(err)
			}
			probe := &memoryPreparationProbe{MemoryPreparationRequester: client, fail: scenario == "outage"}
			server, err := NewServer(reader, identity, fixture.storage, origin)
			if err != nil {
				test.Fatal(err)
			}
			if err := server.SetPreparedMemory(fixture.preparedReader(test, reader)); err != nil {
				test.Fatal(err)
			}
			server.SetMemoryPreparationRequester(probe)
			fixture.mcpHandler = server.Handler()
			version := 1
			args := toolArgs{Mode: "effective", ReadVersion: &version, Ref: string(fixture.selection.SnapshotID), MemoryHash: string(fixture.selection.MemoryHash), CodeCommit: fixture.code}
			request := domain.EffectiveMemoryRequest{Selection: fixture.selection, Limit: 1}
			before := fixture.repositoryState(test)
			if scenario == "revoked" {
				failure := fixture.preparedToolFailure(test, args, "unauthenticated")
				if failure.RetryCursor != "" || failure.Retryable || received.Load() != 1 {
					test.Fatal("revoked forwarding retained permission or pending state")
				}
				if _, err := fixture.service.ReadMemoryPreparationStatus(fixture.ctx, repo, request); !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("revoked request created work", err)
				}
				return
			}
			if scenario == "outage" {
				failure := fixture.preparedToolFailure(test, args, "memory_preparation_unavailable")
				if failure.RetryCursor == "" || !failure.Retryable || received.Load() != 0 {
					test.Fatal("outage lost the fixed minimum retry")
				}
				if _, err := fixture.service.ReadMemoryPreparationStatus(fixture.ctx, repo, request); !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("outage was reported as durably accepted", err)
				}
				args.Cursor = failure.RetryCursor
				probe.fail = false
			}
			wantReceived := int32(1)
			var accepted domain.MemoryReadWork
			if scenario == "acknowledgment_lost" {
				failure := fixture.preparedToolFailure(test, args, "memory_preparation_unavailable")
				if failure.RetryCursor == "" || !failure.Retryable || received.Load() != 1 {
					test.Fatal("lost acknowledgment discarded the fixed minimum")
				}
				accepted, err = fixture.service.ReadMemoryPreparationStatus(fixture.ctx, repo, request)
				if err != nil || accepted.State != "waiting" {
					test.Fatal("lost acknowledgment also lost committed work", err)
				}
				args.Cursor = failure.RetryCursor
				wantReceived++
			}
			pending := fixture.preparedToolFailure(test, args, "memory_read_not_ready")
			if pending.RetryCursor == "" || !pending.Retryable || received.Load() != wantReceived {
				test.Fatal("MCP did not obtain a durable API preparation")
			}
			if args.Cursor != "" && args.Cursor != pending.RetryCursor {
				test.Fatal("outage recovery advanced the initial floor")
			}
			work, err := fixture.service.ReadMemoryPreparationStatus(fixture.ctx, repo, request)
			if err != nil || work.State != "waiting" {
				test.Fatal("cold MCP request did not register API work", err)
			}
			if accepted.ID != "" && accepted != work {
				test.Fatal("acknowledgment retry changed accepted work")
			}
			args.Cursor = pending.RetryCursor
			for duplicate := 0; duplicate < 2; duplicate++ {
				repeated := fixture.preparedToolFailure(test, args, "memory_read_not_ready")
				wantReceived++
				if repeated.RetryCursor != pending.RetryCursor || received.Load() != wantReceived {
					test.Fatal("repeated demand changed the original floor")
				}
				current, err := fixture.service.ReadMemoryPreparationStatus(fixture.ctx, repo, request)
				if err != nil || current != work {
					test.Fatal("repeated demand replaced or advanced existing work", err)
				}
			}
			if err := fixture.service.ProcessMemoryReadPreparations(fixture.ctx, 1); err != nil {
				test.Fatal(err)
			}
			args.Cursor = pending.RetryCursor
			page := fixture.readPreparedTool(test, args)
			if !page.Status.Complete || received.Load() != wantReceived {
				test.Fatal("ready MCP page caused more forwarding")
			}
			if !bytes.Equal(before, fixture.repositoryState(test)) {
				test.Fatal("preparation changed business source")
			}
		})
	}
}
