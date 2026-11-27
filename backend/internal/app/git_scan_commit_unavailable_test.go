package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var errScanCommitUnavailable = fmt.Errorf("%w: %w", outbound.ErrGitCommitUnavailable, outbound.GitProviderStatusError{Status: 404})

func TestGitObservationFailureClassIsBounded(test *testing.T) {
	deadline := &url.Error{Op: "Get", URL: "https://api.github.com/x", Err: context.DeadlineExceeded}
	for failure, want := range map[error]string{
		errors.Join(errScanCommitUnavailable, errors.New("finish failed")): "commit_unavailable_at_provider",
		outbound.GitProviderStatusError{Status: 0}:                         "provider_http_invalid",
		outbound.GitProviderStatusError{Status: 422}:                       "provider_http_422",
		deadline:         "deadline_exceeded",
		context.Canceled: "canceled",
		fmt.Errorf("claim: %w", domain.ErrForbidden):   "forbidden",
		domain.ErrResourceOverloaded:                   "resource_overloaded",
		domain.ErrDocumentIdentityUpgradeRequired:      "document_identity_upgrade_required",
		fmt.Errorf("pg: %w", domain.ErrNotFound):       "not_found",
		errors.New("https://user:token@example.test/"): "unclassified",
	} {
		if got := gitObservationFailureClass(failure); got != want {
			test.Fatalf("class(%v)=%s want %s", failure, got, want)
		}
	}
}
