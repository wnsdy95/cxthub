package http

import (
	"crypto/rand"
	"log/slog"
	"net/http"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/graphdiagnostics"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type graphDiagnosticResponse struct {
	http.ResponseWriter
	recorder *graphdiagnostics.Recorder
	started  time.Time
	status   int
}

func (writer *graphDiagnosticResponse) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *graphDiagnosticResponse) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	if status >= 200 || status == http.StatusSwitchingProtocols {
		writer.status = status
		writer.Header().Add("Server-Timing", writer.recorder.Snapshot().ServerTiming(time.Since(writer.started)))
	}
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *graphDiagnosticResponse) Write(payload []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(payload)
}

func observeGraphRead(writer http.ResponseWriter, request *http.Request) (http.ResponseWriter, *http.Request, func()) {
	logger := slog.Default()
	if !logger.Enabled(request.Context(), slog.LevelInfo) {
		return writer, request, func() {}
	}
	ctx, recorder := graphdiagnostics.NewContext(request.Context())
	requestID := inbound.Correlation(ctx)
	if requestID == "" {
		requestID = rand.Text()
		ctx = inbound.WithCorrelation(ctx, requestID)
	}
	response := &graphDiagnosticResponse{ResponseWriter: writer, recorder: recorder, started: time.Now()}
	response.Header().Set("X-Request-ID", requestID)
	return response, request.WithContext(ctx), func() {
		logger.InfoContext(ctx, "graph_read", "request_id", requestID, "status", response.status,
			"elapsed_ms", float64(time.Since(response.started))/float64(time.Millisecond), "diagnostics", recorder.Snapshot())
	}
}
