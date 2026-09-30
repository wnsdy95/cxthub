package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func diagnosticEvents(c *outbound.SyncDiagnostics, stage outbound.SyncDiagnosticStage) []outbound.SyncDiagnosticEvent {
	var out []outbound.SyncDiagnosticEvent
	for _, e := range c.Snapshot().Events {
		if e.Stage == stage {
			out = append(out, e)
		}
	}
	return out
}

func TestSyncDiagnosticsPreserveWireAndCountEachMeasuredCallOnce(t *testing.T) {
	repoID := string(domain.HashContent([]byte("diagnostic wire fixture")))
	var docs []domain.SessionDoc
	var snapshots []domain.Snapshot
	var canonicalBytes int64
	for _, text := range []string{"PRIVATE_BODY one", "PRIVATE_BODY two"} {
		cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1"}, Events: []domain.Event{
			{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}},
		}}
		canonical, err := domain.CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		id := domain.HashContent(canonical)
		docs = append(docs, domain.SessionDoc{Hash: id, CIR: cir})
		snapshots = append(snapshots, domain.Snapshot{ID: id, RepoID: repoID, Branch: "main", DocHash: id})
		canonicalBytes += int64(len(canonical))
	}
	type request struct{ Method, Path, Body, Auth, Identity string }
	var reference []request
	for _, enabled := range []bool{false, true} {
		var requests []request
		var mu sync.Mutex
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			requests = append(requests, request{r.Method, r.URL.Path, string(raw), r.Header.Get("Authorization"), r.Header.Get("X-Cxt-Identity")})
			mu.Unlock()
			if strings.HasSuffix(r.URL.Path, "/push/negotiate") {
				var req negotiateReq
				if err := json.Unmarshal(raw, &req); err != nil {
					t.Error(err)
				}
				_ = json.NewEncoder(w).Encode(negotiateResp{SnapshotWants: req.SnapshotHaves, DocWants: req.DocHaves})
			} else {
				_, _ = io.WriteString(w, "{}")
			}
		}))
		ctx := context.Background()
		collector := outbound.NewSyncDiagnostics(64)
		if enabled {
			ctx = outbound.WithSyncDiagnostics(ctx, collector)
			ctx = outbound.WithSyncDiagnosticAttempt(ctx, 2)
		}
		c := NewBackendClient(func() string { return ts.URL }, func() string { return "PRIVATE_TOKEN" }, domain.TeamIdentity{Email: "PRIVATE_IDENTITY"})
		_, err := c.NegotiatePushObjects(ctx, repoID, []domain.ContentHash{snapshots[0].ID}, []domain.ContentHash{docs[0].Hash})
		if err == nil {
			err = c.Push(ctx, repoID, snapshots, docs, nil, false, false)
		}
		ts.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !enabled {
			reference = requests
			continue
		}
		if !reflect.DeepEqual(reference, requests) || len(requests) < 3 {
			t.Fatal("diagnostics changed request bodies, headers or publication order")
		}
		for _, request := range requests[2:] {
			if !strings.HasSuffix(request.Path, "/push/objects") {
				t.Fatal("negotiation did not precede object publication")
			}
		}
		totals := map[outbound.SyncDiagnosticStage]outbound.SyncDiagnosticStageTotals{}
		for _, total := range collector.Snapshot().Totals {
			totals[total.Stage] = total
		}
		if totals[outbound.SyncStageCanonical].Calls != 2 || totals[outbound.SyncStageCanonical].Bytes != canonicalBytes || totals[outbound.SyncStageChunkPlan].Calls != 2 || totals[outbound.SyncStageRequestMarshal].Calls != uint64(len(requests)) {
			t.Fatal("counts and durations double-counted a measured phase")
		}
		canonical := diagnosticEvents(collector, outbound.SyncStageCanonical)
		if len(canonical) != 2 || canonical[0].Document != 1 || canonical[1].Document != 2 || canonical[0].Attempt != 2 || canonical[1].Attempt != 2 {
			t.Fatal("direct multi-document push lost its ordinals or retry scope")
		}
		requestsDone := diagnosticEvents(collector, outbound.SyncStageHTTPDo)
		if len(requestsDone) != len(requests) || requestsDone[0].Role != outbound.SyncRoleInventory || requestsDone[1].Role != outbound.SyncRoleDocumentChunks || requestsDone[0].Request == requestsDone[1].Request {
			t.Fatal("inventory and document negotiation were conflated")
		}
		assertDiagnosticPrivacy(t, collector)
	}
}

func waitDiagnosticGate(t *testing.T, gate <-chan struct{}) {
	t.Helper()
	select {
	case <-gate:
	case <-time.After(5 * time.Second):
		t.Fatal("loopback fixture did not reach the controlled stage")
	}
}

func assertDiagnosticPrivacy(t *testing.T, collector *outbound.SyncDiagnostics) {
	t.Helper()
	raw, err := json.Marshal(collector.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"PRIVATE_TOKEN", "PRIVATE_IDENTITY", "PRIVATE_PATH", "PRIVATE_BODY", "PRIVATE_CAUSE", "127.0.0.1"} {
		if strings.Contains(string(raw), text) {
			t.Fatalf("diagnostics exposed %q", text)
		}
	}
}

func TestSyncDiagnosticsTokenBudgetExpiresBeforeTransport(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	collector := outbound.NewSyncDiagnostics(64)
	ctx = outbound.WithSyncDiagnostics(ctx, collector)
	ctx = outbound.WithSyncDiagnosticScope(ctx, outbound.SyncRoleInventory, 1, 0)
	c := NewBackendClient(func() string { return ts.URL }, func() string {
		<-ctx.Done() // Token lookup consumes the existing caller budget.
		return "PRIVATE_TOKEN"
	}, domain.TeamIdentity{Email: "PRIVATE_IDENTITY"})
	var out map[string]any
	err := c.do(ctx, http.MethodPost, "/PRIVATE_PATH", map[string]string{"value": "PRIVATE_BODY"}, &out)
	if !errors.Is(err, context.DeadlineExceeded) || requests.Load() != 0 {
		t.Fatalf("expired lookup reached transport: err=%v requests=%d", err, requests.Load())
	}
	token, request := diagnosticEvents(collector, outbound.SyncStageTokenLookup), diagnosticEvents(collector, outbound.SyncStageHTTPDo)
	if len(token) != 1 || len(request) != 1 || token[0].StartCallerState != outbound.SyncContextActive || token[0].EndCallerState != outbound.SyncContextDeadline || request[0].Outcome != outbound.SyncDiagnosticDeadlineBeforeDo {
		t.Fatal("token exhaustion and Do entry were not distinguished")
	}
	if len(diagnosticEvents(collector, outbound.SyncStageHTTPWroteRequest)) != 0 || len(diagnosticEvents(collector, outbound.SyncStageHTTPFirstByte)) != 0 {
		t.Fatal("Do entry was falsely treated as network activity")
	}
	if c.httpc.Timeout != 30*time.Second || token[0].StartRemainingMillis == nil || *token[0].StartRemainingMillis <= 0 {
		t.Fatal("caller/client budget was changed or not observed")
	}
	assertDiagnosticPrivacy(t, collector)
}

func TestSyncDiagnosticsClientTimeoutBeforeHeaders(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer ts.Close()
	defer close(release)
	collector := outbound.NewSyncDiagnostics(64)
	ctx := outbound.WithSyncDiagnostics(context.Background(), collector)
	var priorTrace atomic.Int32
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { priorTrace.Add(1) }})
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "PRIVATE_TOKEN" }, domain.TeamIdentity{})
	c.httpc.Timeout = 150 * time.Millisecond // Test-only; production remains 30s.
	var out map[string]any
	err := c.do(ctx, http.MethodPost, "/PRIVATE_PATH", map[string]string{"value": "PRIVATE_BODY"}, &out)
	waitDiagnosticGate(t, entered)
	if err == nil || ctx.Err() != nil {
		t.Fatal("fixture must expire the HTTP client with an active caller")
	}
	request := diagnosticEvents(collector, outbound.SyncStageHTTPDo)
	if len(request) != 1 || request[0].Outcome != outbound.SyncDiagnosticTimeoutActive || request[0].EndCallerState != outbound.SyncContextActive || request[0].Counts.ClientTimeoutMillis != 150 {
		t.Fatal("HTTP timeout was mislabeled as exhausted caller budget")
	}
	if priorTrace.Load() != 1 || len(diagnosticEvents(collector, outbound.SyncStageHTTPWroteRequest)) != 1 || len(diagnosticEvents(collector, outbound.SyncStageHTTPFirstByte)) != 0 || len(diagnosticEvents(collector, outbound.SyncStageHTTPBody)) != 0 {
		t.Fatal("trace composition or pre-header boundary changed")
	}
	assertDiagnosticPrivacy(t, collector)
}

func TestSyncDiagnosticsHeadersThenBodyTimeout(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer ts.Close()
	defer close(release)
	collector := outbound.NewSyncDiagnostics(64)
	ctx := outbound.WithSyncDiagnostics(context.Background(), collector)
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	c.httpc.Timeout = 150 * time.Millisecond
	var out map[string]any
	if err := c.do(ctx, http.MethodGet, "/PRIVATE_PATH", nil, &out); err == nil {
		t.Fatal("stalled JSON body succeeded")
	}
	request, body := diagnosticEvents(collector, outbound.SyncStageHTTPDo), diagnosticEvents(collector, outbound.SyncStageHTTPBody)
	if len(request) != 1 || len(body) != 1 || request[0].Outcome != outbound.SyncDiagnosticSuccess || body[0].Outcome != outbound.SyncDiagnosticTimeoutActive || body[0].Counts.HTTPStatus != 200 || body[0].DurationMillis <= 0 {
		t.Fatal("successful headers and failed response body were conflated")
	}
	if len(diagnosticEvents(collector, outbound.SyncStageHTTPFirstByte)) != 1 || request[0].Request != body[0].Request || ctx.Err() != nil {
		t.Fatal("body failure lost its request scope or changed caller context")
	}
	assertDiagnosticPrivacy(t, collector)
}

type diagnosticReadGate struct {
	io.ReadCloser
	read chan struct{}
	once sync.Once
}

func (r *diagnosticReadGate) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.read) })
	return r.ReadCloser.Read(p)
}

func TestSyncDiagnosticsCancellationBoundaries(t *testing.T) {
	for _, stage := range []string{"before_do", "during_do", "during_body"} {
		t.Run(stage, func(t *testing.T) {
			entered, read := make(chan struct{}), make(chan struct{})
			release := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				if stage == "during_body" {
					_, _ = io.WriteString(w, "{")
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer ts.Close()
			defer close(release)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			cause := errors.New("PRIVATE_CAUSE")
			collector := outbound.NewSyncDiagnostics(64)
			ctx = outbound.WithSyncDiagnostics(ctx, collector)
			c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
			transport := &http.Transport{}
			defer transport.CloseIdleConnections()
			c.httpc.Transport = ackResponseTransportFunc(func(req *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(req)
				if err == nil && stage == "during_body" {
					response.Body = &diagnosticReadGate{ReadCloser: response.Body, read: read}
				}
				return response, err
			})
			if stage == "before_do" {
				cancel(cause)
			}
			done := make(chan error, 1)
			go func() {
				var out map[string]any
				done <- c.do(ctx, http.MethodGet, "/PRIVATE_PATH", nil, &out)
			}()
			if stage == "during_do" {
				waitDiagnosticGate(t, entered)
				cancel(cause)
			} else if stage == "during_body" {
				waitDiagnosticGate(t, read)
				cancel(cause)
			}
			select {
			case err := <-done:
				if !errors.Is(err, cause) || ctx.Err() != context.Canceled {
					t.Fatalf("cancellation chain changed: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("canceled operation did not finish")
			}
			observedStage := outbound.SyncStageHTTPDo
			if stage == "during_body" {
				observedStage = outbound.SyncStageHTTPBody
			}
			observed := diagnosticEvents(collector, observedStage)
			if len(observed) != 1 || observed[0].Outcome != outbound.SyncDiagnosticCanceled {
				t.Fatal("cancellation boundary was not observed")
			}
			assertDiagnosticPrivacy(t, collector)
		})
	}
}

func TestSyncDiagnosticsIgnoredACKDoesNotInventFailure(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer ts.Close()
	defer close(release)
	collector := outbound.NewSyncDiagnostics(64)
	ctx := outbound.WithSyncDiagnostics(context.Background(), collector)
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	if err := c.do(ctx, http.MethodPost, "/PRIVATE_PATH", nil, nil); err != nil {
		t.Fatal(err)
	}
	if collector.HasFailures() || ctx.Err() != nil {
		t.Fatal("successful acknowledged write was reclassified by best-effort cleanup")
	}
	body := diagnosticEvents(collector, outbound.SyncStageHTTPBody)
	if len(body) != 1 || body[0].Outcome != outbound.SyncDiagnosticSuccess || body[0].EndCallerState != outbound.SyncContextActive {
		t.Fatal("ACK cancellation was attributed to the caller")
	}
}
