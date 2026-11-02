package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strconv"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (server *Server) SetPreparedMemory(reader inbound.PreparedMemoryReader) error {
	if reader == nil {
		return domain.ErrValidation
	}
	switch value := reflect.ValueOf(reader); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return domain.ErrValidation
		}
	}
	server.preparedMemory = reader
	return nil
}

func (server *Server) queryPreparedMemory(writer http.ResponseWriter, request *http.Request) {
	query := server.preparedMemory
	if query == nil {
		server.writeError(writer, http.StatusServiceUnavailable, "prepared_memory_unavailable", "Prepared memory read unavailable")
		return
	}
	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || (values.Has("cursor") && values.Get("cursor") == "") {
		server.respond(writer, nil, domain.ErrValidation)
		return
	}
	for _, name := range []string{"branch", "snapshot_id", "code_commit", "memory_hash", "content", "cursor", "limit"} {
		if len(values[name]) > 1 {
			server.respond(writer, nil, domain.ErrValidation)
			return
		}
	}
	input := domain.EffectiveMemoryRequest{
		Selection: domain.EffectiveMemorySelection{
			Branch:     values.Get("branch"),
			SnapshotID: domain.ContentHash(values.Get("snapshot_id")),
			CodeCommit: values.Get("code_commit"),
			MemoryHash: domain.ContentHash(values.Get("memory_hash")),
		},
		Content: values.Get("content"),
		Cursor:  values.Get("cursor"),
	}
	if values.Has("limit") {
		var err error
		input.Limit, err = strconv.Atoi(values.Get("limit"))
		if err != nil || input.Limit < 1 || input.Limit > 50 {
			server.respond(writer, nil, domain.ErrValidation)
			return
		}
	}
	if input.Selection.Validate() != nil || len(input.Cursor) > domain.MaxPreparedMemoryCursorBytes ||
		(input.Content != "" && input.Content != "all" && input.Content != "claims" && input.Content != "prompt") {
		server.respond(writer, nil, domain.ErrValidation)
		return
	}
	server.memoryReadWithScope(writer, request, preparedMemoryReadScope{scope: query}, func(ctx context.Context) (any, error) {
		page, err := query.QueryPreparedMemory(ctx, server.repoID(request), input)
		return page, preparedMemoryError(err)
	})
}

type preparedMemoryReadScope struct {
	scope inbound.MemoryReadScope
}

func (scope preparedMemoryReadScope) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return preparedMemoryError(scope.scope.WithMemoryReadResources(ctx, repo, read))
}

type preparedMemoryHTTPError struct {
	cause error
}

func (failure *preparedMemoryHTTPError) Error() string { return failure.cause.Error() }
func (failure *preparedMemoryHTTPError) Unwrap() error { return failure.cause }

func preparedMemoryError(err error) error {
	if err == nil {
		return nil
	}
	return &preparedMemoryHTTPError{cause: err}
}

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
