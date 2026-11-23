package http

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type admissionReadBackend struct{ Backend }

func admissionReadResult[Result any](ctx context.Context) (Result, error) {
	var result Result
	_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
	if err != nil {
		return result, err
	}
	defer release()
	return result, nil
}

func (admissionReadBackend) GetDoc(ctx context.Context, _, _ domain.ContentHash) (domain.SessionDoc, error) {
	return admissionReadResult[domain.SessionDoc](ctx)
}

func (admissionReadBackend) ReadDocViewEvents(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int, int, string) (domain.DocViewPage, error) {
	return domain.DocViewPage{Next: -1}, nil
}

type admissionUnreadBody struct{ reads int }

func (body *admissionUnreadBody) Read([]byte) (int, error) { body.reads++; return 0, io.EOF }
func (*admissionUnreadBody) Close() error                  { return nil }

type admissionResponseWriter struct {
	*httptest.ResponseRecorder
	checked  bool
	err      error
	writeErr error
}

func (writer *admissionResponseWriter) Write(payload []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
	if release != nil {
		release()
	}
	writer.checked, writer.err = true, err
	if writer.writeErr != nil {
		return 0, writer.writeErr
	}
	return writer.ResponseRecorder.Write(payload)
}

func TestResourceAdmissionHTTPRetryableErrors(t *testing.T) {
	for _, sentinel := range []error{domain.ErrResourceOverloaded, domain.ErrDocRangeIndexUnavailable} {
		response := httptest.NewRecorder()
		(&Server{}).respond(response, nil, fmt.Errorf("wrapped: %w", sentinel))
		if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
			t.Fatalf("not retryable: %v code=%d headers=%v", sentinel, response.Code, response.Header())
		}
	}
	response := httptest.NewRecorder()
	(&Server{}).respond(response, nil, fmt.Errorf("wrapped: %w", domain.ErrDocRangeIndexPending))
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "3" || !strings.Contains(response.Body.String(), "doc_range_index_pending") {
		t.Fatalf("pending range retry contract: code=%d headers=%v body=%s", response.Code, response.Header(), response.Body)
	}
}
