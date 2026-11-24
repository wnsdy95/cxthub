package http

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type admissionUnreadBody struct{ reads int }

func (body *admissionUnreadBody) Read([]byte) (int, error) { body.reads++; return 0, io.EOF }
func (*admissionUnreadBody) Close() error                  { return nil }

func TestResourceAdmissionHTTPRetryableErrors(t *testing.T) {
	for _, sentinel := range []error{domain.ErrResourceOverloaded, domain.ErrDocRangeIndexUnavailable} {
		response := httptest.NewRecorder()
		(&Server{}).respond(response, nil, fmt.Errorf("wrapped: %w", sentinel))
		if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
			t.Fatalf("not retryable: %v code=%d headers=%v", sentinel, response.Code, response.Header())
		}
	}
}
