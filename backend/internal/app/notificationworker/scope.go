package notificationworker

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Scope interface {
	WorkerContext(context.Context) context.Context
}

type RepositoryReader interface {
	GetRepository(context.Context, string) (domain.Repository, error)
}

type Config struct {
	Scope        Scope
	Jobs         outbound.NotificationStore
	Repositories RepositoryReader
	Guard        outbound.NotificationDeliveryGuard
	Sender       outbound.NotificationSender
}
