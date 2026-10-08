package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Development only: job transitions persist atomically, but enqueue cannot join
// a multi-file business transaction. Production uses the PostgreSQL outbox.
func (s *FSStore) notificationPath(id string) string {
	return filepath.Join(s.dataDir, "notification-outbox", opaqueName(id)+".json")
}
func (s *FSStore) writeNotification(d outbound.NotificationDelivery) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return writeAtomic(s.notificationPath(d.Job.ID), b)
}
func (s *FSStore) EnqueueNotification(ctx context.Context, d outbound.NotificationDelivery) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	if _, err := os.Stat(s.notificationPath(d.Job.ID)); err == nil {
		return domain.ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.writeNotification(d)
}
func (s *FSStore) notificationsRaw() ([]outbound.NotificationDelivery, error) {
	es, err := os.ReadDir(filepath.Join(s.dataDir, "notification-outbox"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []outbound.NotificationDelivery{}
	for _, e := range es {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var d outbound.NotificationDelivery
		if err = readJSON(filepath.Join(s.dataDir, "notification-outbox", e.Name()), &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].Job.CreatedAt.Equal(out[k].Job.CreatedAt) {
			return out[i].Job.ID < out[k].Job.ID
		}
		return out[i].Job.CreatedAt.Before(out[k].Job.CreatedAt)
	})
	return out, nil
}
func (s *FSStore) ListNotifications(ctx context.Context, repository string) ([]domain.NotificationJob, error) {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	ds, err := s.notificationsRaw()
	if err != nil {
		return nil, err
	}
	out := []domain.NotificationJob{}
	for i := len(ds) - 1; i >= 0; i-- {
		if ds[i].Job.RepositoryID == repository {
			out = append(out, ds[i].Job)
			if len(out) == 100 {
				break
			}
		}
	}
	return out, nil
}
func (s *FSStore) ClaimNotification(ctx context.Context, now time.Time, lease time.Duration) (outbound.NotificationDelivery, error) {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	ds, err := s.notificationsRaw()
	if err != nil {
		return outbound.NotificationDelivery{}, err
	}
	for _, d := range ds {
		j := &d.Job
		if !((j.State == "pending" || j.State == "retrying") && !j.NextAttempt.After(now) || j.State == "running" && !j.LeaseUntil.After(now)) {
			continue
		}
		release, policyErr := s.pinWorkerRepositoryPolicy(ctx, j.RepositoryID)
		if errors.Is(policyErr, domain.ErrDocumentIdentityUpgradeRequired) {
			continue
		}
		if policyErr != nil {
			return outbound.NotificationDelivery{}, policyErr
		}
		defer release()
		j.State = "running"
		j.Attempts++
		j.Version++
		j.UpdatedAt = now
		j.LeaseUntil = now.Add(lease)
		return d, s.writeNotification(d)
	}
	return outbound.NotificationDelivery{}, domain.ErrNotFound
}
func (s *FSStore) FinishNotification(ctx context.Context, j domain.NotificationJob, now time.Time) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	var d outbound.NotificationDelivery
	if err := readJSON(s.notificationPath(j.ID), &d); err != nil {
		return err
	}
	if d.Job.State != "running" || d.Job.Version != j.Version || !d.Job.LeaseUntil.After(now) {
		return domain.ErrRefConflict
	}
	if !notificationPayloadMatches(d.Job, j) {
		return domain.ErrRefConflict
	}
	release, err := s.pinWorkerRepositoryPolicy(ctx, d.Job.RepositoryID)
	if err != nil {
		return err
	}
	defer release()
	d.Job = j
	return s.writeNotification(d)
}
func (s *FSStore) RetryNotification(ctx context.Context, repository, id, destination string, now time.Time) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	var d outbound.NotificationDelivery
	if err := readJSON(s.notificationPath(id), &d); err != nil {
		return err
	}
	j := &d.Job
	if j.RepositoryID != repository {
		return domain.ErrNotFound
	}
	if j.State == "delivered" || j.State == "running" && j.LeaseUntil.After(now) {
		return domain.ErrConflict
	}
	j.State = "pending"
	j.Attempts = 0
	j.Version++
	j.Reason = ""
	j.HTTPStatus = 0
	j.NextAttempt = now
	j.UpdatedAt = now
	j.LeaseUntil = time.Time{}
	d.Destination = destination
	return s.writeNotification(d)
}

var _ outbound.NotificationStore = (*FSStore)(nil)

func (s *FSStore) ValidateNotificationDelivery(ctx context.Context, claimed outbound.NotificationDelivery, now time.Time) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	var current outbound.NotificationDelivery
	if err := readJSON(s.notificationPath(claimed.Job.ID), &current); err != nil {
		return err
	}
	release, err := s.pinWorkerRepositoryPolicy(ctx, current.Job.RepositoryID)
	if err != nil {
		return err
	}
	defer release()
	repo, err := s.GetRepository(ctx, current.Job.RepositoryID)
	if err != nil {
		return err
	}
	// Lock waits and configuration reads may outlive the caller's sample.
	// Preserve later caller times while requiring the lease to be live now.
	if currentTime := time.Now().UTC(); currentTime.After(now) {
		now = currentTime
	}
	return checkNotificationDelivery(current, claimed, repo, now)
}

var _ outbound.NotificationDeliveryGuard = (*FSStore)(nil)
