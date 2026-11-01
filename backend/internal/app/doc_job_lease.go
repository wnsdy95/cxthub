package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentworker"
)

func withDocJobLease(ctx context.Context, lease time.Duration, renew func(context.Context) error, run func(context.Context) error) error {
	return documentworker.WithLease(ctx, lease, renew, run)
}
