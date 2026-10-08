package store

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"time"
)

func notificationPayloadMatches(a, b domain.NotificationJob) bool {
	return a.ID == b.ID && a.RepositoryID == b.RepositoryID && a.Kind == b.Kind &&
		a.Text == b.Text && a.Attempts == b.Attempts && a.Version == b.Version && a.CreatedAt.Equal(b.CreatedAt)
}
func checkNotificationDelivery(current, claimed outbound.NotificationDelivery, repo domain.Repository, now time.Time) error {
	a, b := current.Job, claimed.Job
	if !notificationPayloadMatches(a, b) || a.State != "running" || b.State != "running" ||
		!a.LeaseUntil.After(now) || !a.LeaseUntil.Equal(b.LeaseUntil) ||
		!a.UpdatedAt.Equal(b.UpdatedAt) || !a.NextAttempt.Equal(b.NextAttempt) ||
		a.Reason != b.Reason || a.HTTPStatus != b.HTTPStatus || current.Destination != claimed.Destination {
		return domain.ErrRefConflict
	}
	if repo.ID != a.RepositoryID || repo.Archived || repo.WebhookURL == "" || repo.WebhookURL != current.Destination {
		return domain.ErrForbidden
	}
	return nil
}
