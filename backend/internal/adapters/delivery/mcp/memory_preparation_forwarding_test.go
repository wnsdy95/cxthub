package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryPreparationRequesterFunc func(context.Context, string, domain.MemoryReadPreparationRequest) error

func (requester memoryPreparationRequesterFunc) RequestMemoryPreparation(ctx context.Context, token string, request domain.MemoryReadPreparationRequest) error {
	return requester(ctx, token, request)
}

func TestMemoryPreparationForwardingRejectsMixedPending(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	version := 1
	args := toolArgs{Mode: "effective", ReadVersion: &version, Ref: string(fixture.snapshot.ID), CodeCommit: strings.Repeat("a", 40)}
	fixture.server.SetMemoryPreparationRequester(memoryPreparationRequesterFunc(func(context.Context, string, domain.MemoryReadPreparationRequest) error {
		test.Fatal("mixed pending forwarded an API request")
		return nil
	}))
	for _, wrapped := range []bool{false, true} {
		pending := error(&domain.MemoryReadPending{Cursor: "pending"})
		if wrapped {
			prepared := preparedMemoryFailure(pending).(*preparedMemoryToolError)
			prepared.RetryCursor = "old-retry"
			prepared.preparation = &domain.MemoryReadPreparationRequest{Version: 1, RepoID: fixture.repo.ID,
				Selection: domain.EffectiveMemorySelection{SnapshotID: fixture.snapshot.ID, CodeCommit: args.CodeCommit}, Cursor: "pending"}
			pending = prepared
		}
		for _, failure := range []error{errors.Join(pending, errors.New("unmapped storage failure")), fmt.Errorf("outer: %w", errors.Join(errors.New("private failure"), fmt.Errorf("inner: %w", pending)))} {
			requirePreparedMemoryReader(test, fixture.server, preparedMemoryReader(fixture.backend, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
				return domain.MemoryReadPage{}, failure
			}))
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), args))
			body := response.Body.String()
			if !strings.Contains(body, `"code":"unavailable"`) || strings.Contains(body, "retry_cursor") || strings.Contains(body, "private failure") {
				test.Fatal("unknown failure became pending or leaked details", body)
			}
		}
	}
}

func TestMemoryPreparationForwardingFailureKeepsOnlyRetryableFloor(test *testing.T) {
	request := domain.MemoryReadPreparationRequest{Version: 1, RepoID: domain.HashContent([]byte("repo")),
		Selection: domain.EffectiveMemorySelection{SnapshotID: domain.HashContent([]byte("snapshot")), CodeCommit: strings.Repeat("a", 40)}, Cursor: "inner-pending"}
	for _, cause := range []error{nil, domain.ErrMemoryPreparationUnavailable, domain.ErrResourceOverloaded, context.DeadlineExceeded, domain.ErrForbidden, domain.ErrIntegrity, domain.ErrMemoryCaptureExpired, domain.ErrCatalogResetRequired} {
		calls := 0
		server := &Server{memoryPreparation: memoryPreparationRequesterFunc(func(_ context.Context, token string, input domain.MemoryReadPreparationRequest) error {
			calls++
			if token != "bearer" || input.Cursor != request.Cursor || input.Selection != request.Selection || input.RepoID != request.RepoID {
				test.Fatal("forwarding changed credential or selection")
			}
			return cause
		})}
		pending := &preparedMemoryToolError{Code: "memory_read_not_ready", Retryable: true, RetryCursor: "outer-pending", cause: domain.ErrMemoryReadNotReady, preparation: &request}
		actual := server.forwardMemoryPreparation(context.Background(), "bearer", pending)
		if calls != 1 || (cause == nil && actual != pending) {
			test.Fatal("forwarding did not return original accepted pending")
		}
		if cause != nil && (!errors.Is(actual, cause) || (actual.Retryable && actual.RetryCursor != pending.RetryCursor) || (!actual.Retryable && actual.RetryCursor != "")) {
			test.Fatal("forwarding failure lost terminal semantics or retry floor", actual)
		}
	}
}
