package outbound

import (
	"context"
	"time"
)

// ValidateNotificationDelivery rechecks the exact live claim, payload,
// destination and current repository compatibility immediately before egress.
// The boundary ends before network I/O; this is not a durable send receipt.
type NotificationDeliveryGuard interface {
	ValidateNotificationDelivery(context.Context, NotificationDelivery, time.Time) error
}
