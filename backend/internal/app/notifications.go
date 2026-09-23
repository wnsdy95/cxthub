package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const notificationMaxAttempts = 8
const notificationLease = 2 * time.Minute

// ProcessNotification never keeps a database transaction open across network IO.
// The stable event ID lets compatible receivers deduplicate lost acknowledgments.
func (s *Service) ProcessNotification(ctx context.Context) (bool, error) {
	st, ok := s.meta.(outbound.NotificationStore)
	if !ok || s.repositories == nil {
		return false, nil
	}
	d, err := st.ClaimNotification(ctx, time.Now().UTC(), notificationLease)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	j := d.Job
	finish := func(state, reason string, status int, delay time.Duration) (bool, error) {
		now := time.Now().UTC()
		j.State = state
		j.Reason = reason
		j.HTTPStatus = status
		j.UpdatedAt = now
		j.LeaseUntil = time.Time{}
		j.NextAttempt = now.Add(delay)
		return true, st.FinishNotification(ctx, j, now)
	}
	retry := func(reason string, status int, delay time.Duration) (bool, error) {
		if j.Attempts >= notificationMaxAttempts {
			return finish("attention", "attempts_exhausted", status, 0)
		}
		backoff := time.Duration(1<<min(j.Attempts, 10)) * 15 * time.Second
		if delay < backoff {
			delay = backoff
		}
		if delay > 24*time.Hour {
			delay = 24 * time.Hour
		}
		return finish("retrying", reason, status, delay)
	}
	if j.Attempts > notificationMaxAttempts {
		return finish("attention", "attempts_exhausted", 0, 0)
	}
	repositoryRecord, err := s.repositories.GetRepository(ctx, j.RepositoryID)
	if errors.Is(err, domain.ErrNotFound) {
		return finish("attention", "destination_disabled", 0, 0)
	}
	if err != nil {
		return retry("configuration_unavailable", 0, 0)
	}
	if repositoryRecord.Archived || repositoryRecord.WebhookURL == "" {
		return finish("attention", "destination_disabled", 0, 0)
	}
	if repositoryRecord.WebhookURL != d.Destination {
		return finish("attention", "destination_changed", 0, 0)
	}
	if !webhookSchemeOK(d.Destination) {
		return finish("attention", "invalid_destination", 0, 0)
	}
	body, err := json.Marshal(map[string]string{"text": j.Text})
	if err != nil {
		return retry("encoding_failed", 0, 0)
	}
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, d.Destination, bytes.NewReader(body))
	if err != nil {
		return finish("attention", "invalid_destination", 0, 0)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CXTHub-Event-ID", j.ID)
	resp, err := safeWebhookClient().Do(req)
	if err != nil {
		return retry("transport_failed", 0, 0)
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return finish("delivered", "", resp.StatusCode, 0)
	}
	if resp.StatusCode == 408 || resp.StatusCode == 425 || resp.StatusCode == 429 || resp.StatusCode >= 500 {
		delay := time.Duration(0)
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			delay = time.Duration(min(seconds, 86400)) * time.Second
		} else if at, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			delay = time.Until(at)
		}
		return retry("http_retryable", resp.StatusCode, delay)
	}
	return finish("attention", "http_rejected", resp.StatusCode, 0)
}

func (s *Service) RunNotificationWorker(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			for i := 0; i < 20; i++ {
				worked, err := s.ProcessNotification(ctx)
				if err != nil || !worked {
					break
				}
			}
			timer.Reset(3 * time.Second)
		}
	}
}

func (s *IdentityService) notificationManager(ctx context.Context, user, repository string) (domain.Repository, error) {
	role, member := s.RoleOf(ctx, repository, user)
	if !member || !role.AtLeast(domain.RoleMaintainer) {
		return domain.Repository{}, domain.ErrForbidden
	}
	return s.repositories.GetRepository(ctx, repository)
}
func (s *IdentityService) ListNotifications(ctx context.Context, user, repository string) ([]domain.NotificationJob, error) {
	if _, err := s.notificationManager(ctx, user, repository); err != nil {
		return nil, err
	}
	st, ok := s.repositories.(outbound.NotificationStore)
	if !ok {
		return nil, fmt.Errorf("notification storage unavailable")
	}
	return st.ListNotifications(ctx, repository)
}
func (s *IdentityService) RetryNotification(ctx context.Context, user, repository, id string) error {
	return s.withIdentity(ctx, func(ctx context.Context) error { return s.retryNotification(ctx, user, repository, id) })
}

func (s *IdentityService) retryNotification(ctx context.Context, user, repository, id string) error {
	repositoryRecord, err := s.notificationManager(ctx, user, repository)
	if err != nil {
		return err
	}
	if repositoryRecord.Archived || repositoryRecord.WebhookURL == "" {
		return fmt.Errorf("%w: configure an active webhook first", domain.ErrConflict)
	}
	st, ok := s.repositories.(outbound.NotificationStore)
	if !ok {
		return fmt.Errorf("notification storage unavailable")
	}
	return st.RetryNotification(ctx, repository, id, repositoryRecord.WebhookURL, time.Now().UTC())
}
