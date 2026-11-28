package app

import (
	"context"
	"errors"
	"net"
	"strconv"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// gitObservationFailureClass names a failure from a fixed vocabulary. Error
// text is never logged: provider errors can carry URLs.
func gitObservationFailureClass(err error) string {
	var status outbound.GitProviderStatusError
	var network net.Error
	switch {
	case errors.Is(err, outbound.ErrGitCommitUnavailable):
		return "commit_unavailable_at_provider"
	case errors.Is(err, outbound.ErrGitProviderRateLimited):
		return "provider_rate_limited"
	case errors.As(err, &status):
		if status.Status < 100 || status.Status > 599 {
			return "provider_http_invalid"
		}
		return "provider_http_" + strconv.Itoa(status.Status)
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, domain.ErrResourceOverloaded):
		return "resource_overloaded"
	case errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired):
		return "document_identity_upgrade_required"
	case errors.Is(err, domain.ErrIntegrity):
		return "integrity"
	case errors.Is(err, domain.ErrValidation):
		return "validation"
	case errors.Is(err, domain.ErrConflict):
		return "conflict"
	case errors.Is(err, domain.ErrForbidden):
		return "forbidden"
	case errors.Is(err, domain.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, domain.ErrNotFound):
		return "not_found"
	case errors.As(err, &network):
		return "network"
	}
	return "unclassified"
}
