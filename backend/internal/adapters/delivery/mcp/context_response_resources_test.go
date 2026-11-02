package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func contextResponseBulkHeld(test *testing.T) {
	test.Helper()
	probe, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, release, err := app.AcquireResources(probe, app.ResourceBulk)
	if release != nil {
		release()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		test.Error("context response escaped existing bulk admission", err)
	}
}

func contextResponseBulkAvailable(test *testing.T) {
	test.Helper()
	probe, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, release, err := app.AcquireResources(probe, app.ResourceBulk)
	if err != nil {
		test.Fatal("context response leaked existing bulk admission", err)
	}
	release()
}

func contextResponseReadHeld(test *testing.T) {
	test.Helper()
	probe, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, release, err := app.AcquireResources(probe, app.ResourceInteractive)
	if release != nil {
		release()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		test.Error("context response did not retain its interactive slot", err)
	}
}

func TestMCPContextResponseDoesNotWaitForOccupiedBulk(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	_, releaseOther, err := app.AcquireResources(context.Background(), app.ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer releaseOther()
	calls := 0
	setContextQueryTestPort(test, fixture.server, contextMCPQueryFunc(func(ctx context.Context, _ domain.ContentHash, _ domain.ContextSelection) (domain.ContextQueryView, error) {
		calls++
		contextResponseReadHeld(test)
		bound, release, err := app.AcquireResources(ctx, app.ResourceInteractive)
		if err != nil {
			return domain.ContextQueryView{}, err
		}
		defer release()
		return domain.ContextQueryView{Version: 1, Position: fixture.snapshot.ID, Snapshots: []domain.Snapshot{fixture.snapshot}}, bound.Err()
	}))
	writes := 0
	writer := &documentResponseWriter{ResponseRecorder: httptest.NewRecorder(), write: func() {
		writes++
		contextResponseReadHeld(test)
	}}
	fixture.server.Handler().ServeHTTP(writer, documentEventRequest(test, context.Background(), fixture.token, "context_list", toolArgs{Repository: string(fixture.repo.ID)}))
	if calls != 1 || writes != 1 || writer.Code != http.StatusOK || strings.Contains(writer.Body.String(), `"isError":true`) {
		test.Fatal("ready context metadata waited for an unrelated bulk job", calls, writes, writer.Body)
	}
	releaseOther()
	contextResponseBulkAvailable(test)
}

func TestMCPContextResponseRetainsResourcesThroughProtocolWrite(test *testing.T) {
	for _, mode := range []string{"", "segments"} {
		for _, outcome := range []string{"complete", "cancel", "write failure", "borrowed", "not ready"} {
			test.Run(mode+"/"+outcome, func(test *testing.T) {
				fixture := newMemoryAdmissionFixture(test)
				_, releaseOther, err := app.AcquireResources(context.Background(), app.ResourceInteractive)
				if err != nil {
					test.Fatal(err)
				}
				defer releaseOther()
				parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				releaseBorrowed := func() {}
				if outcome == "borrowed" {
					parent, releaseBorrowed, err = app.AcquireResources(parent, app.ResourceBulk)
					if err != nil {
						test.Fatal(err)
					}
					defer releaseBorrowed()
				}
				calls := 0
				var queryContext context.Context
				expectedSegmentLimit := 0
				if mode == "segments" {
					expectedSegmentLimit = 1
				}
				setContextQueryTestPort(test, fixture.server, contextMCPQueryFunc(func(ctx context.Context, repo domain.ContentHash, selection domain.ContextSelection) (domain.ContextQueryView, error) {
					calls++
					queryContext = ctx
					if repo != fixture.repo.ID || selection.SegmentLimit != expectedSegmentLimit {
						test.Error("context response changed repository or selection", repo, selection)
					}
					if deadline, present := ctx.Deadline(); !present || !deadline.Equal(contextResponseDeadline(parent)) {
						test.Error("context query lost the earlier request deadline", deadline)
					}
					for _, class := range []app.ResourceClass{app.ResourceInteractive, app.ResourceBulk} {
						bound, release, err := app.AcquireResources(ctx, class)
						if err != nil {
							return domain.ContextQueryView{}, err
						}
						if bound.Err() != nil {
							test.Error("nested context read lost live ownership", bound.Err())
						}
						release()
					}
					contextResponseBulkHeld(test)
					if outcome == "not ready" {
						return domain.ContextQueryView{}, domain.ErrAgentHistoryUnavailable
					}
					view := domain.ContextQueryView{Version: 1, Position: fixture.snapshot.ID, Snapshots: []domain.Snapshot{fixture.snapshot}}
					if mode == "segments" {
						entry := domain.ContextSegmentCoverage{SnapshotID: fixture.snapshot.ID, Kind: "full_source", Reason: strings.Repeat("retained metadata ", 2000)}
						view.Segments = &domain.ContextSegmentPage{Version: domain.ContextSegmentVersion, StateHash: pageHash(4), PageHash: pageHash(5), Next: -1, Total: 1, Complete: true, Entries: []domain.ContextSegmentCoverage{entry}}
					}
					return view, nil
				}))
				writes := 0
				writer := &documentResponseWriter{ResponseRecorder: httptest.NewRecorder(), write: func() {
					writes++
					contextResponseBulkHeld(test)
					if outcome == "cancel" {
						cancel()
						contextResponseBulkHeld(test)
					}
				}}
				if outcome == "write failure" {
					writer.cause = io.ErrClosedPipe
				}
				fixture.server.Handler().ServeHTTP(writer, documentEventRequest(test, parent, fixture.token, "context_list", toolArgs{Repository: string(fixture.repo.ID), Mode: mode}))
				if calls != 1 || writes != 1 || len(writer.deadlines) != 2 {
					test.Fatal("query, final protocol writer or transport deadline missing", calls, writes, writer.deadlines)
				}
				for _, deadline := range writer.deadlines {
					if !deadline.Equal(contextResponseDeadline(parent)) {
						test.Error("transport extended the earlier request deadline", deadline)
					}
				}
				if queryContext.Err() == nil {
					test.Error("response did not cancel its owned request context")
				}
				if outcome != "cancel" && parent.Err() != nil {
					test.Error("response canceled the caller context", parent.Err())
				}
				if outcome == "complete" || outcome == "borrowed" || outcome == "not ready" {
					var response struct {
						ID     int `json:"id"`
						Result struct {
							IsError bool `json:"isError"`
							Content []struct {
								Text string `json:"text"`
							} `json:"content"`
						} `json:"result"`
					}
					if err := json.Unmarshal(writer.Body.Bytes(), &response); err != nil {
						test.Fatal(err)
					}
					if writer.Code != http.StatusOK || response.ID != 1 || response.Result.IsError != (outcome == "not ready") || len(response.Result.Content) != 1 {
						test.Fatal("context response changed JSON-RPC result", writer.Code, writer.Body)
					}
					if outcome == "not ready" && !strings.Contains(response.Result.Content[0].Text, domain.ErrAgentHistoryUnavailable.Error()) {
						test.Error("context readiness error changed", writer.Body)
					}
				}
				if outcome == "borrowed" {
					contextResponseBulkHeld(test)
					releaseBorrowed()
				}
				contextResponseBulkAvailable(test)
			})
		}
	}
}

func contextResponseDeadline(ctx context.Context) time.Time {
	deadline, _ := ctx.Deadline()
	return deadline
}

func TestMCPContextResponseNotification(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	_, releaseOther, err := app.AcquireResources(context.Background(), app.ResourceInteractive)
	if err != nil {
		test.Fatal(err)
	}
	defer releaseOther()
	calls := 0
	setContextQueryTestPort(test, fixture.server, contextMCPQueryFunc(func(ctx context.Context, _ domain.ContentHash, _ domain.ContextSelection) (domain.ContextQueryView, error) {
		calls++
		contextResponseReadHeld(test)
		return domain.ContextQueryView{Version: 1}, nil
	}))
	body := `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"context_list","arguments":{"repository":"` + string(fixture.repo.ID) + `"}}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	for key, value := range remoteMCPHeaders(fixture.token) {
		request.Header.Set(key, value)
	}
	writer := &documentResponseWriter{ResponseRecorder: httptest.NewRecorder(), write: func() { test.Error("notification wrote a response body") }}
	fixture.server.Handler().ServeHTTP(writer, request)
	if writer.Code != http.StatusAccepted || calls != 1 {
		test.Fatal("context notification changed", writer.Code, calls)
	}
	contextResponseBulkAvailable(test)
}

func TestMCPContextResponseDoesNotGateOrdinaryMethods(test *testing.T) {
	for _, method := range []string{"initialize", "ping", "tools/list", "notifications/initialized", "notifications/cancelled", "unknown", "repository_list", "context_history"} {
		test.Run(method, func(test *testing.T) {
			fixture := newMemoryAdmissionFixture(test)
			setContextQueryTestPort(test, fixture.server, contextMCPQueryFunc(func(ctx context.Context, _ domain.ContentHash, _ domain.ContextSelection) (domain.ContextQueryView, error) {
				if _, present := ctx.Deadline(); present {
					test.Error("ordinary context history gained a response deadline")
				}
				return domain.ContextQueryView{Version: 1}, nil
			}))
			_, release, err := app.AcquireResources(context.Background(), app.ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer release()
			request := documentEventRequest(test, context.Background(), fixture.token, method, toolArgs{Repository: string(fixture.repo.ID)})
			if method != "repository_list" && method != "context_history" {
				body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: method})
				if err != nil {
					test.Fatal(err)
				}
				request.Body = io.NopCloser(strings.NewReader(string(body)))
			}
			writes := 0
			writer := &documentResponseWriter{ResponseRecorder: httptest.NewRecorder(), write: func() { writes++ }}
			fixture.server.Handler().ServeHTTP(writer, request)
			if writer.Code != http.StatusOK || writes != 1 || len(writer.deadlines) != 0 || strings.Contains(writer.Body.String(), `"isError":true`) {
				test.Fatal("ordinary method gained response admission or deadlines", writer.Code, writer.deadlines, writer.Body)
			}
		})
	}
}
