package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"time"
)

// RuntimeStore shares expiring authorization and request admission state.
type RuntimeStore interface {
	CreateDevicePairing(context.Context, domain.DevicePairing) error
	GetDevicePairing(context.Context, string, string, time.Time) (domain.DevicePairing, error)
	ApproveDevicePairing(context.Context, string, string, time.Time) error
	// Redemption and insertion of the hashed session share one database transaction.
	RedeemDevicePairing(context.Context, string, string, domain.Session, time.Time) error
	// GCRA admission: burst up to limit, refill at limit/window. Zero now uses the store clock.
	AllowRequest(context.Context, string, int, time.Duration, time.Time) (bool, error)
	PruneRuntimeState(context.Context, time.Time) error
}
