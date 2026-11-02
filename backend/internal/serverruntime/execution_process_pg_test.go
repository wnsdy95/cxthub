//go:build postgres

package serverruntime_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	goRuntime "runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	delivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/http"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/mcpserver"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
	"github.com/wnsdy95/cxthub/backend/internal/workerserver"
)

type executionProcessCommand struct {
	Operation string
	Token     string
	Bulk      bool
	TimeoutMS int
}

type executionProcessEvent struct {
	State      string
	Token      string
	Code       string
	PID        int
	URL        string
	Ready      bool
	Canceled   bool
	Joined     bool
	Closed     bool
	HeapAlloc  uint64
	HeapSys    uint64
	RuntimeSys uint64
}

type executionProcessHold struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

func executionProcessErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, domain.ErrValidation):
		return "validation"
	case errors.Is(err, errors.ErrUnsupported), errors.Is(err, domain.ErrCatalogUnsupported):
		return "unsupported"
	case errors.Is(err, domain.ErrConflict):
		return "conflict"
	case errors.Is(err, domain.ErrResourceOverloaded):
		return "overloaded"
	default:
		return "unexpected"
	}
}

func TestBoundedExecutionProcessPGChild(test *testing.T) {
	role := outbound.ProcessRole(os.Getenv("CXT_WAVE39_PROCESS_ROLE"))
	if role == "" {
		test.Skip("entered only by the isolated bounded-profile process proofs")
	}
	if role != outbound.ProcessAPI && role != outbound.ProcessMCP && role != outbound.ProcessWorker {
		test.Fatal("invalid fixture role")
	}
	configuration := executionProcessLocalConfig(test, os.Getenv("CXT_POSTGRES_DSN"))
	database := os.Getenv("CXT_WAVE39_PROCESS_DATABASE")
	if !strings.HasPrefix(database, "bounded_process_") || configuration.ConnConfig.Database != database {
		test.Fatal("child requires its parent's explicit fresh owned local database")
	}
	lifetime, stopLifetime := context.WithTimeout(context.Background(), 35*time.Second)
	defer stopLifetime()
	signaled, stopSignals := signal.NotifyContext(lifetime, syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	var output sync.Mutex
	emit := func(event executionProcessEvent) {
		event.PID = os.Getpid()
		encoded, err := json.Marshal(event)
		if err != nil {
			test.Error("encode private child protocol")
			return
		}
		output.Lock()
		defer output.Unlock()
		_, _ = fmt.Fprintf(os.Stdout, "CXT_W39 %s\n", encoded)
	}
	var runtime *serverruntime.Runtime
	var worker *serverruntime.WorkerRuntime
	var err error
	switch role {
	case outbound.ProcessAPI:
		runtime, err = serverruntime.Open(signaled, "127.0.0.1:0", os.Getenv("HOME"), true)
	case outbound.ProcessMCP:
		runtime, err = serverruntime.OpenMCP(signaled, "127.0.0.1:0", os.Getenv("HOME"), true)
	case outbound.ProcessWorker:
		if os.Getenv("CXT_EXECUTION_PROFILE") == outbound.BoundedWorkersProfile {
			worker, err = workerserver.OpenWorker(signaled)
		} else {
			worker, err = serverruntime.OpenDocumentWorker(signaled)
		}
	}
	if err != nil {
		emit(executionProcessEvent{State: "refused", Code: executionProcessErrorCode(err)})
		return
	}
	process := signaled
	closeRoot := func() error {
		if worker != nil {
			return worker.Close()
		}
		runtime.Close()
		return nil
	}
	defer func() { _ = closeRoot() }()
	var server *httptest.Server
	if worker != nil {
		process = worker.ProcessContext(signaled)
	} else {
		if !runtime.BoundedDocuments() || (runtime.Context != nil) != (role == outbound.ProcessAPI) {
			test.Fatal("real root did not retain the selected profile and command boundary")
		}
		process = runtime.ProcessContext(signaled)
		var handler http.Handler
		if role == outbound.ProcessAPI {
			api := delivery.NewServer(runtime.Context, runtime.Identity)
			if err := api.SetDocumentQueries(runtime.DocumentQueries, runtime.DocumentQueryProfile); err != nil {
				test.Fatal("install actual API document queries")
			}
			handler = runtime.HealthHandler(api.Handler())
		} else {
			handler, err = mcpserver.Handler(runtime, runtime.PublicURL)
			if err != nil {
				test.Fatal("assemble actual independent MCP protocol root")
			}
		}
		server = httptest.NewUnstartedServer(handler)
		server.Config.BaseContext = executionProcessBaseContext(process)
		server.Start()
		defer server.Close()
	}
	opened := executionProcessEvent{State: "opened"}
	if server != nil {
		opened.URL = server.URL
	}
	emit(opened)
	commands := make(chan executionProcessCommand)
	go func() {
		defer close(commands)
		decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
		for {
			var command executionProcessCommand
			if decoder.Decode(&command) != nil {
				return
			}
			select {
			case commands <- command:
			case <-lifetime.Done():
				return
			}
		}
	}()
	holds := make(map[string]*executionProcessHold)
	var running chan error
	for process.Err() == nil {
		select {
		case <-process.Done():
		case command, present := <-commands:
			if !present {
				stopSignals()
				continue
			}
			switch command.Operation {
			case "metrics":
				var statistics goRuntime.MemStats
				goRuntime.ReadMemStats(&statistics)
				emit(executionProcessEvent{State: "metrics", Token: command.Token,
					HeapAlloc: statistics.HeapAlloc, HeapSys: statistics.HeapSys, RuntimeSys: statistics.Sys})
			case "ready":
				ready := worker != nil && worker.Ready()
				if server != nil {
					response := httptest.NewRecorder()
					runtime.HealthHandler(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil).WithContext(process))
					ready = response.Code == http.StatusOK
				}
				emit(executionProcessEvent{State: "ready", Token: command.Token, Ready: ready})
			case "hold":
				if command.Token == "" || holds[command.Token] != nil || command.TimeoutMS < 1 || command.TimeoutMS > 10000 {
					test.Fatal("invalid hold command")
				}
				hold := &executionProcessHold{stop: make(chan struct{}), done: make(chan struct{})}
				holds[command.Token] = hold
				go func(command executionProcessCommand, hold *executionProcessHold) {
					defer close(hold.done)
					ctx, cancel := context.WithTimeout(process, time.Duration(command.TimeoutMS)*time.Millisecond)
					defer cancel()
					class := app.ResourceInteractive
					if command.Bulk {
						class = app.ResourceBulk
					}
					emit(executionProcessEvent{State: "waiting", Token: command.Token})
					bound, release, err := app.AcquireResources(ctx, class)
					if err != nil {
						emit(executionProcessEvent{State: "rejected", Token: command.Token, Code: executionProcessErrorCode(err)})
						return
					}
					emit(executionProcessEvent{State: "acquired", Token: command.Token})
					state := "released"
					select {
					case <-hold.stop:
					case <-bound.Done():
					}
					if bound.Err() != nil {
						state = "canceled"
					}
					release()
					emit(executionProcessEvent{State: state, Token: command.Token})
				}(command, hold)
			case "release":
				hold := holds[command.Token]
				if hold == nil {
					test.Fatal("release does not name an owned hold")
				}
				hold.once.Do(func() { close(hold.stop) })
			case "run":
				if worker == nil || running != nil {
					test.Fatal("only the actual worker root may run its document loops")
				}
				running = make(chan error, 1)
				go func() { running <- worker.Run(process) }()
				emit(executionProcessEvent{State: "running"})
			default:
				test.Fatal("unsupported private process command")
			}
		}
	}
	for _, hold := range holds {
		hold.once.Do(func() { close(hold.stop) })
		select {
		case <-hold.done:
		case <-lifetime.Done():
			test.Fatal("admitted child work did not join")
		}
	}
	if running != nil {
		select {
		case err := <-running:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				test.Fatal("actual worker Run returned an unexpected error class")
			}
		case <-lifetime.Done():
			test.Fatal("actual worker Run did not join")
		}
	}
	if server != nil {
		server.Close()
	}
	if err := closeRoot(); err != nil {
		test.Fatal("actual root close failed")
	}
	emit(executionProcessEvent{State: "closed", Canceled: process.Err() != nil, Joined: true, Closed: true})
	select {
	case command, present := <-commands:
		if !present || command.Operation != "exit" {
			test.Fatal("parent did not acknowledge close while child remained alive")
		}
	case <-lifetime.Done():
		test.Fatal("close acknowledgement exceeded the child lifetime")
	}
}

func TestBoundedExecutionProcessesPGAdmissionAndWorker(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture := newExecutionProcessFixture(test, ctx)
	api := fixture.start(test, ctx, outbound.ProcessAPI)
	api.expect(test, ctx, "opened", "")
	api.assertReady(test, ctx, false)
	api.reject(test, ctx, "incomplete-roster", false)
	mcp := fixture.start(test, ctx, outbound.ProcessMCP)
	mcp.expect(test, ctx, "opened", "")
	mcp.assertReady(test, ctx, false)
	worker := fixture.start(test, ctx, outbound.ProcessWorker)
	worker.expect(test, ctx, "opened", "")
	for _, child := range []*executionProcessChild{api, mcp, worker} {
		child.waitReady(test, ctx, true)
	}
	fixture.assertSessions(test, ctx, 3, 0)
	for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
		duplicate := fixture.start(test, ctx, role)
		if event := duplicate.expect(test, ctx, "refused", ""); event.Code != "conflict" {
			test.Fatal("duplicate real root was not refused as a role conflict", role, event.Code)
		}
		duplicate.join(test, ctx, false)
	}
	api.hold(test, ctx, "api-bulk", true)
	mcp.reject(test, ctx, "mcp-shared-busy", true)
	worker.hold(test, ctx, "worker-reserved-bulk", true)
	api.hold(test, ctx, "api-interactive", false)
	mcp.hold(test, ctx, "mcp-interactive", false)
	fixture.assertSessions(test, ctx, 3, 2)
	for _, child := range []*executionProcessChild{api, mcp, worker} {
		child.reject(test, ctx, "active-limit", false)
	}
	test.Log("four held admissions observed in three real runtime processes: API2 + MCP1 + worker1; two granted session bulk locks")
	api.release(test, ctx, "api-bulk")
	mcp.release(test, ctx, "mcp-interactive")
	mcp.hold(test, ctx, "mcp-bulk", true)
	api.reject(test, ctx, "api-shared-busy", true)
	fixture.waitRejectedBulkConnectionClosed(test, ctx, outbound.ProcessAPI)
	fixture.assertSessions(test, ctx, 3, 2)
	mcp.release(test, ctx, "mcp-bulk")
	worker.release(test, ctx, "worker-reserved-bulk")
	api.release(test, ctx, "api-interactive")
	fixture.assertSessions(test, ctx, 3, 0)
	worker.send(test, executionProcessCommand{Operation: "run"})
	worker.expect(test, ctx, "running", "")
	fixture.waitDocumentJob(test, ctx)
	fixture.publishSnapshot(test, ctx)
	fixture.assertDocumentReads(test, ctx, api, mcp)
	worker.terminateAndAcknowledge(test, ctx, fixture)
	api.waitReady(test, ctx, false)
	mcp.waitReady(test, ctx, false)
	api.reject(test, ctx, "worker-role-lost", false)
	mcp.reject(test, ctx, "worker-role-lost", true)
	fixture.assertSessions(test, ctx, 2, 0)
	api.terminateAndAcknowledge(test, ctx, fixture)
	mcp.terminateAndAcknowledge(test, ctx, fixture)
	fixture.assertSessions(test, ctx, 0, 0)
}

func (fixture *executionProcessFixture) waitRejectedBulkConnectionClosed(test *testing.T, ctx context.Context, role outbound.ProcessRole) {
	test.Helper()
	deadline, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var remaining int
		err := fixture.observer.QueryRow(deadline, `SELECT count(*) FROM pg_stat_activity
 WHERE datname=current_database() AND application_name=$1`, "cxthub/bounded-documents-v1/"+string(role)+"/bulk").Scan(&remaining)
		if err != nil {
			test.Fatal("observe rejected request's discarded bulk connection", role, err)
		}
		if remaining == 0 {
			return
		}
		select {
		case <-deadline.Done():
			test.Fatal("rejected request's bulk connection survived the original one-second discard bound", role, remaining)
		case <-ticker.C:
		}
	}
}

func TestBoundedExecutionProcessesPGControlLossAndKill(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture := newExecutionProcessFixture(test, ctx)
	api := fixture.start(test, ctx, outbound.ProcessAPI)
	api.expect(test, ctx, "opened", "")
	mcp := fixture.start(test, ctx, outbound.ProcessMCP)
	mcp.expect(test, ctx, "opened", "")
	worker := fixture.start(test, ctx, outbound.ProcessWorker)
	worker.expect(test, ctx, "opened", "")
	for _, child := range []*executionProcessChild{api, mcp, worker} {
		child.waitReady(test, ctx, true)
	}
	api.hold(test, ctx, "control-loss-live-work", true)
	mcp.send(test, executionProcessCommand{Operation: "hold", Token: "blocked-on-api", Bulk: true, TimeoutMS: 5000})
	mcp.expect(test, ctx, "waiting", "blocked-on-api")
	fixture.terminateControl(test, ctx, outbound.ProcessAPI)
	api.expect(test, ctx, "canceled", "control-loss-live-work")
	api.acknowledgeClosed(test, ctx, fixture)
	if event := mcp.expect(test, ctx, "rejected", "blocked-on-api"); event.Code != "overloaded" && event.Code != "canceled" {
		test.Fatal("blocked peer acquired after control/role loss", event.Code)
	}
	mcp.waitReady(test, ctx, false)
	worker.waitReady(test, ctx, false)
	worker.reject(test, ctx, "missing-api", true)
	fixture.assertSessions(test, ctx, 2, 0)
	api = fixture.start(test, ctx, outbound.ProcessAPI)
	api.expect(test, ctx, "opened", "")
	for _, child := range []*executionProcessChild{api, mcp, worker} {
		child.waitReady(test, ctx, true)
	}
	worker.hold(test, ctx, "kill-live-bulk", true)
	if err := worker.command.Process.Kill(); err != nil {
		test.Fatal("kill only the owned worker child")
	}
	worker.join(test, ctx, true)
	fixture.waitRoleSessionsGone(test, ctx, outbound.ProcessWorker)
	api.waitReady(test, ctx, false)
	mcp.waitReady(test, ctx, false)
	api.reject(test, ctx, "killed-worker", true)
	fixture.assertSessions(test, ctx, 2, 0)
	worker = fixture.start(test, ctx, outbound.ProcessWorker)
	worker.expect(test, ctx, "opened", "")
	for _, child := range []*executionProcessChild{api, mcp, worker} {
		child.waitReady(test, ctx, true)
	}
	worker.hold(test, ctx, "replacement-worker-bulk", true)
	fixture.assertSessions(test, ctx, 3, 1)
	worker.release(test, ctx, "replacement-worker-bulk")
	worker.terminateAndAcknowledge(test, ctx, fixture)
	api.terminateAndAcknowledge(test, ctx, fixture)
	mcp.terminateAndAcknowledge(test, ctx, fixture)
	fixture.assertSessions(test, ctx, 0, 0)
}
