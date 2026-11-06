package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/graphdiagnostics"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func graphDiagnosticLog(test *testing.T, level slog.Level) *bytes.Buffer {
	test.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: level})))
	test.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func TestGraphDiagnosticsHTTPLogHeaderAndRedaction(test *testing.T) {
	output := graphDiagnosticLog(test, slog.LevelInfo)
	server := &Server{repositoryGraphs: &repositoryGraphHTTPQueries{}}
	request := httptest.NewRequest(http.MethodGet, "/private-repository?position=secret-position", strings.NewReader("secret-request-body"))
	request.Header.Set("Authorization", "Bearer secret-token")
	request.Header.Set("X-Request-ID", "secret-client-id")
	response := &admissionDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	server.graphRead(response, request, func(request *http.Request) (any, error) {
		ctx, span := graphdiagnostics.Start(request.Context(), graphdiagnostics.Snapshots)
		graphdiagnostics.SelectRoute(ctx, graphdiagnostics.Legacy)
		graphdiagnostics.RecordCache(ctx, graphdiagnostics.GraphCache, graphdiagnostics.Hit)
		graphdiagnostics.StartSQL(ctx).End(5, true, false)
		span.End()
		return map[string]string{"body": "secret-response-body"}, nil
	})
	if response.Code != http.StatusOK || response.Body.String() != "{\"body\":\"secret-response-body\"}\n" || response.read.IsZero() || response.write.IsZero() {
		test.Fatal("telemetry changed response or deadlines", response)
	}
	requestID := response.Header().Get("X-Request-ID")
	if requestID == "" || requestID == "secret-client-id" {
		test.Fatal("request ID was not server generated", requestID)
	}
	var record struct {
		Message     string `json:"msg"`
		RequestID   string `json:"request_id"`
		Status      int    `json:"status"`
		Diagnostics struct {
			Route string                    `json:"route"`
			SQL   graphdiagnostics.SQLStats `json:"sql"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		test.Fatal("expected one structured record", output.String(), err)
	}
	if record.Message != "graph_read" || record.RequestID != requestID || record.Status != 200 || record.Diagnostics.Route != "legacy" || record.Diagnostics.SQL.Rows != 5 {
		test.Fatal("missing request diagnostics", record)
	}
	header := response.Result().Header.Get("Server-Timing")
	if !strings.Contains(header, "query;dur=") || !strings.Contains(header, "snapshots;dur=") || !strings.Contains(header, "graph_pre;dur=") {
		test.Fatal("timing unavailable before response", header)
	}
	for _, secret := range []string{"secret-", "private-repository", "Authorization", "Bearer"} {
		if strings.Contains(output.String(), secret) || strings.Contains(header, secret) {
			test.Fatal("diagnostics leaked request/response content", output.String(), header)
		}
	}
}

func TestGraphDiagnosticsHTTPReusesTrustedCorrelation(test *testing.T) {
	output := graphDiagnosticLog(test, slog.LevelInfo)
	server := &Server{repositoryGraphs: &repositoryGraphHTTPQueries{}}
	var trusted string
	handler := server.withCorrelation(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		trusted = inbound.Correlation(request.Context())
		server.graphRead(writer, request, func(request *http.Request) (any, error) {
			if inbound.Correlation(request.Context()) != trusted {
				test.Fatal("trusted middleware correlation replaced")
			}
			return nil, nil
		})
	}))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Request-ID", "secret-client-id")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if trusted == "" || response.Header().Get("X-Request-ID") != trusted || !strings.Contains(output.String(), trusted) || strings.Contains(output.String(), "secret-client-id") {
		test.Fatal("correlation changed or client ID was echoed", response.Header(), output.String())
	}
}

func TestGraphDiagnosticsHTTPFailuresAndDisabledObserver(test *testing.T) {
	for _, failure := range []struct {
		name   string
		graphs bool
		cause  error
		status int
	}{
		{name: "unavailable", status: http.StatusNotImplemented},
		{name: "read error", graphs: true, cause: errors.Join(domain.ErrForbidden, errors.New("secret-error")), status: http.StatusForbidden},
		{name: "canceled", graphs: true, cause: context.Canceled, status: http.StatusInternalServerError},
	} {
		test.Run(failure.name, func(test *testing.T) {
			output := graphDiagnosticLog(test, slog.LevelInfo)
			server := &Server{}
			if failure.graphs {
				server.repositoryGraphs = &repositoryGraphHTTPQueries{}
			}
			response := httptest.NewRecorder()
			server.graphRead(response, httptest.NewRequest(http.MethodGet, "/", nil), func(*http.Request) (any, error) { return nil, failure.cause })
			if response.Code != failure.status || response.Result().Header.Get("Server-Timing") == "" || strings.Contains(output.String(), "secret-error") || !strings.Contains(output.String(), "graph_read") {
				test.Fatal("error semantics/telemetry changed", response.Code, output.String())
			}
		})
	}
	test.Run("disabled", func(test *testing.T) {
		output := graphDiagnosticLog(test, slog.LevelWarn)
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		writer, observed, finish := observeGraphRead(response, request)
		finish()
		if writer != response || observed != request || graphdiagnostics.StartSQL(observed.Context()) != nil || len(response.Header()) != 0 || output.Len() != 0 {
			test.Fatal("disabled telemetry changed request/response")
		}
	})
}

func TestGraphDiagnosticsHTTPStreamsAndFreezesPreResponseTiming(test *testing.T) {
	graphDiagnosticLog(test, slog.LevelInfo)
	response := &admissionDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	writer, request, finish := observeGraphRead(response, httptest.NewRequest(http.MethodGet, "/", nil))
	defer finish()
	deadline := time.Now().Add(time.Minute)
	if err := http.NewResponseController(writer).SetWriteDeadline(deadline); err != nil || !response.write.Equal(deadline) {
		test.Fatal("response controller did not unwrap", err)
	}
	_, before := graphdiagnostics.Start(request.Context(), graphdiagnostics.Metadata)
	before.End()
	if written, err := writer.Write([]byte("first")); err != nil || written != 5 || response.Body.String() != "first" {
		test.Fatal("response was buffered", written, err)
	}
	header := response.Result().Header.Get("Server-Timing")
	_, after := graphdiagnostics.Start(request.Context(), graphdiagnostics.Repair)
	after.End()
	writer.WriteHeader(http.StatusBadRequest)
	_, _ = writer.Write([]byte("second"))
	if response.Code != http.StatusOK || response.Body.String() != "firstsecond" || response.Header().Get("Server-Timing") != header || !strings.Contains(header, "metadata;dur=") || strings.Contains(header, "repair;dur=") {
		test.Fatal("late work changed committed response", response.Code, response.Header())
	}
}
