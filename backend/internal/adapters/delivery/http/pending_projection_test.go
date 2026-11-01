package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPendingProjectionHTTPRetryContract(t *testing.T) {
	for _, failure := range []error{domain.ErrPendingProjectionNotReady, errors.Join(errors.New("wrapped"), domain.ErrPendingProjectionNotReady), domain.ErrPendingProjectionFailed, domain.ErrForbidden, domain.ErrIntegrity} {
		authorizerPresent := false
		queries := &repositoryStateHTTPQueries{err: failure, read: func(ctx context.Context, _ domain.ContentHash) {
			authorizerPresent = inbound.DocumentReadAuthorizer(ctx) != nil
		}}
		server := NewServer(nil, nil)
		if err := server.SetRepositoryStateQueries(queries, inbound.RepositoryStateCompatibility); err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.pendingView(response, httptest.NewRequest(http.MethodGet, "/?projection=on-hold-v1", nil))
		if !authorizerPresent {
			t.Fatal("fresh request authorization not bound")
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if errors.Is(failure, domain.ErrPendingProjectionNotReady) {
			if response.Code != 503 || response.Header().Get("Retry-After") != "1" || body.Error.Code != "pending_projection_pending" {
				t.Fatal("catch-up retry contract lost", response.Code, response.Body)
			}
		} else if body.Error.Code == "pending_projection_pending" || response.Header().Get("Retry-After") != "" {
			t.Fatal("terminal failure disguised as pending", failure, response.Code, response.Body)
		}
	}
}
