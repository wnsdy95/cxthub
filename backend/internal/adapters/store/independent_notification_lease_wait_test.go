package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// A synthetic short lease makes the existing queue-lock wait deterministic.
// No network transport, PostgreSQL, or product modification is involved.
func TestIndependentFSNotificationGuardLeaseWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st := NewFSStore(t.TempDir())
	r := domain.Repository{ID: domain.NewID("ws_"), Name: "synthetic", OwnerID: "dev:notification-review", Visibility: domain.VisibilityPrivate, CreatedAt: time.Now().UTC(), WebhookURL: "https://example.test/unused"}
	if err := st.CreateRepository(ctx, r); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte("independent-notification-lease-wait"))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: r.ID}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	j := domain.NotificationJob{ID: domain.NewID("evt_"), RepositoryID: r.ID, Kind: "synthetic", Text: "synthetic metadata", State: "pending", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
	if err := st.EnqueueNotification(ctx, outbound.NotificationDelivery{Job: j, Destination: r.WebhookURL}); err != nil {
		t.Fatal(err)
	}
	d, err := st.ClaimNotification(ctx, time.Now().UTC(), 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ValidateNotificationDelivery(ctx, d, time.Now().UTC()); err != nil {
		t.Fatalf("live-lease positive control: %v", err)
	}
	lock := st.oauthLock()
	lock.Lock()
	locked := true
	defer func() {
		if locked {
			lock.Unlock()
		}
	}()
	entered := make(chan time.Time, 1)
	done := make(chan error, 1)
	go func() {
		at := time.Now().UTC()
		entered <- at
		done <- st.ValidateNotificationDelivery(ctx, d, at)
	}()
	admittedAt := <-entered
	if !admittedAt.Before(d.Job.LeaseUntil) {
		t.Fatal("invalid probe: guard entry was already expired")
	}
	select {
	case err := <-done:
		t.Fatalf("invalid probe: guard bypassed the held queue lock: %v", err)
	case <-time.After(time.Until(d.Job.LeaseUntil) + 40*time.Millisecond):
	}
	lock.Unlock()
	locked = false
	err = <-done
	returnedAt := time.Now().UTC()
	if !returnedAt.After(d.Job.LeaseUntil) {
		t.Fatal("invalid probe: return did not cross expiry")
	}
	freshErr := st.ValidateNotificationDelivery(ctx, d, returnedAt)
	if !errors.Is(freshErr, domain.ErrRefConflict) {
		t.Fatalf("fresh-time expired-lease control: %v", freshErr)
	}
	t.Logf("guard_wait_ms=%.3f returned_after_expiry_ms=%.3f stale_time_result=%v fresh_time_result=%v", float64(returnedAt.Sub(admittedAt))/float64(time.Millisecond), float64(returnedAt.Sub(d.Job.LeaseUntil))/float64(time.Millisecond), err, freshErr)
	if !errors.Is(err, domain.ErrRefConflict) {
		t.Fatalf("expired lease admitted after queue lock wait: got %v, want ErrRefConflict", err)
	}
}
