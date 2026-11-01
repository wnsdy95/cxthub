package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type DeviceLoginClient interface {
	Start(context.Context, string) (domain.DeviceLoginAuthorization, error)
	Poll(context.Context, string, string) (domain.DeviceLoginApproval, error)
	VerifyToken(context.Context, string) (string, error)
}
