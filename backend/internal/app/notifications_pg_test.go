//go:build postgres

package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type failedNotificationPG struct{ *store.PostgresStore }

func (s failedNotificationPG) EnqueueNotification(ctx context.Context, d outbound.NotificationDelivery) error {
	if err := s.PostgresStore.EnqueueNotification(ctx, d); err != nil {
		return err
	}
	return errors.New("injected failure after outbox insert")
}
