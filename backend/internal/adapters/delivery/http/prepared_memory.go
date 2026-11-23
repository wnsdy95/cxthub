package http

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type preparedMemoryHTTPError struct {
	cause error
}

func (failure *preparedMemoryHTTPError) Error() string { return failure.cause.Error() }
func (failure *preparedMemoryHTTPError) Unwrap() error { return failure.cause }

func (server *Server) writePreparedMemoryError(writer http.ResponseWriter, err error) {
	code, status := mapError(err)
	message := "Prepared memory read failed"
	retryCursor := ""
	switch {
	case errors.Is(err, domain.ErrUnauthorized):
		code, status, message = "unauthenticated", http.StatusUnauthorized, domain.ErrUnauthorized.Error()
	case errors.Is(err, domain.ErrForbidden):
		code, status, message = "forbidden", http.StatusForbidden, domain.ErrForbidden.Error()
	case errors.Is(err, domain.ErrIntegrity):
		code, status, message = "integrity_violation", http.StatusUnprocessableEntity, domain.ErrIntegrity.Error()
	case errors.Is(err, domain.ErrMemoryReadNeedsAttention):
		code, status, message = "memory_read_needs_attention", http.StatusServiceUnavailable, domain.ErrMemoryReadNeedsAttention.Error()
	case errors.Is(err, domain.ErrMemoryCaptureExpired):
		code, status, message = "memory_capture_expired", http.StatusGone, domain.ErrMemoryCaptureExpired.Error()
	case errors.Is(err, domain.ErrCatalogResetRequired):
		code, status, message = "catalog_reset_required", http.StatusConflict, domain.ErrCatalogResetRequired.Error()
	case errors.Is(err, domain.ErrEffectiveMemoryCursorStale):
		code, status, message = "memory_cursor_stale", http.StatusConflict, domain.ErrEffectiveMemoryCursorStale.Error()
	case errors.Is(err, domain.ErrCatalogUnsupported):
		code, status, message = "prepared_memory_unavailable", http.StatusServiceUnavailable, "Prepared memory read unavailable"
	case domain.IsOnlyMemoryReadPending(err) && code == "internal":
		code, status, message = "memory_read_not_ready", http.StatusServiceUnavailable, domain.ErrMemoryReadNotReady.Error()
		writer.Header().Set("Retry-After", "2")
		var pending *domain.MemoryReadPending
		if errors.As(err, &pending) && pending != nil && len(pending.Cursor) <= domain.MaxPreparedMemoryCursorBytes {
			retryCursor = pending.Cursor
		}
	}
	if retryCursor == "" {
		server.writeError(writer, status, code, message)
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": message, "details": map[string]any{"retry_cursor": retryCursor}},
	})
}
