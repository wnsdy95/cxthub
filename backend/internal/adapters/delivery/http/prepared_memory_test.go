package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type preparedMemoryScopeFunc func(context.Context, domain.ContentHash, func(context.Context) error) error

func (scope preparedMemoryScopeFunc) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope(ctx, repo, read)
}

func assertPreparedMemoryHTTPError(test *testing.T, response *httptest.ResponseRecorder, status int, code, retryAfter, retryCursor string) {
	test.Helper()
	var envelope struct {
		Error struct {
			Code    string            `json:"code"`
			Message string            `json:"message"`
			Details map[string]string `json:"details"`
		} `json:"error"`
	}
	decoder := json.NewDecoder(strings.NewReader(response.Body.String()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		test.Fatal(err)
	}
	if response.Code != status || envelope.Error.Code != code || response.Header().Get("Retry-After") != retryAfter || envelope.Error.Details["retry_cursor"] != retryCursor || envelope.Error.Message == "" {
		test.Fatalf("error contract: status=%d retry=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body)
	}
	if retryCursor == "" && len(envelope.Error.Details) != 0 || retryCursor != "" && len(envelope.Error.Details) != 1 {
		test.Fatalf("unexpected error details: %s", response.Body)
	}
	for _, secret := range []string{"private", "Private prepared memory", "prepared-replay", "must-not-leak"} {
		if strings.Contains(response.Body.String(), secret) {
			test.Fatalf("error leaked payload or internal details: %s", response.Body)
		}
	}
}

func TestPreparedMemoryHTTPLegacyErrorMappingUnchanged(test *testing.T) {
	server := &Server{}
	for _, failure := range []error{
		domain.ErrMemoryReadNotReady,
		&domain.MemoryReadPending{Cursor: "must-not-leak"},
		domain.ErrMemoryReadNeedsAttention,
		domain.ErrMemoryCaptureExpired,
		domain.ErrCatalogResetRequired,
		domain.ErrCatalogUnsupported,
	} {
		response := httptest.NewRecorder()
		server.respond(response, nil, fmt.Errorf("private wrapped payload: %w", failure))
		assertPreparedMemoryHTTPError(test, response, http.StatusInternalServerError, "internal", "", "")
	}
}
