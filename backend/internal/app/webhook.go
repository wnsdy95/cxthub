package app

import (
	"context"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Notifications contain event metadata only, never secrets ciphertext or plaintext.
func enqueueRepositoryNotification(ctx context.Context, store any, repositoryRecord domain.Repository, kind, text string) error {
	if repositoryRecord.WebhookURL == "" || repositoryRecord.Archived {
		return nil
	}
	outbox, ok := store.(outbound.NotificationStore)
	if !ok {
		return fmt.Errorf("durable notification storage unavailable")
	}
	return enqueueRepositoryNotificationWithQueue(ctx, outbox, repositoryRecord, kind, text)
}

type RepositoryNotificationQueue interface {
	EnqueueNotification(context.Context, outbound.NotificationDelivery) error
}

func enqueueRepositoryNotificationWithQueue(ctx context.Context, outbox RepositoryNotificationQueue, repositoryRecord domain.Repository, kind, text string) error {
	if repositoryRecord.WebhookURL == "" || repositoryRecord.Archived {
		return nil
	}
	if outbox == nil {
		return fmt.Errorf("durable notification storage unavailable")
	}
	now := time.Now().UTC()
	return outbox.EnqueueNotification(ctx, outbound.NotificationDelivery{Destination: repositoryRecord.WebhookURL, Job: domain.NotificationJob{
		ID: domain.NewID("evt_"), RepositoryID: repositoryRecord.ID, Kind: kind, Text: text, State: "pending", CreatedAt: now, UpdatedAt: now, NextAttempt: now,
	}})
}
