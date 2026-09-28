package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type CaptureRecovery interface {
	RecordedResolutions(context.Context, string) (map[string]domain.CaptureResolution, error)
	Inspect(context.Context, string) ([]domain.CaptureRecoveryStatus, error)
	Resolve(context.Context, string, string, domain.ContentHash, string, bool) (domain.CaptureResolution, error)
	RetryAttempt(context.Context, string, string, domain.ContentHash) (domain.CaptureAttempt, error)
}
