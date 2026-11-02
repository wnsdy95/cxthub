package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestMemoryReadAuditFailureDoesNotExposeCause(test *testing.T) {
	const private = "private-audit-database-location"
	response := httptest.NewRecorder()
	NewServer(nil, nil).respond(response, nil, errors.Join(domain.ErrBreakGlassAuditUnavailable, errors.New(private)))
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), private) ||
		!strings.Contains(response.Body.String(), `"code":"audit_unavailable"`) || !strings.Contains(response.Body.String(), `"message":"break-glass audit unavailable"`) {
		test.Fatalf("unsafe audit failure: %d %s", response.Code, response.Body)
	}
}
