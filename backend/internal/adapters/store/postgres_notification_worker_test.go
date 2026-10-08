//go:build postgres

package store

import (
	"errors"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"testing"
	"time"
)

func TestWorkerPGNotificationPolicy(t *testing.T) {
	st, ctx := chunkReusePG(t)
	user := domain.User{ID: domain.NewID("user_"), Username: fmt.Sprintf("worker%d", time.Now().UnixNano()), Name: "Synthetic", Email: "worker@example.test"}
	if e := st.UpsertUser(ctx, user); e != nil {
		t.Fatal(e)
	}
	record := domain.Repository{ID: domain.NewID("ws_"), Name: "Synthetic", OwnerID: user.ID, OwnerUsername: user.Username, Slug: "worker", CreatedAt: time.Now().UTC(), WebhookURL: "https://example.test/synthetic"}
	if e := st.CreateRepository(ctx, record); e != nil {
		t.Fatal(e)
	}
	repo := domain.HashContent([]byte(record.ID))
	if _, e := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID}); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	j := domain.NotificationJob{ID: domain.NewID("evt_"), RepositoryID: record.ID, Kind: "synthetic", Text: "metadata", State: "pending", CreatedAt: now, UpdatedAt: now, NextAttempt: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)}
	if e := st.EnqueueNotification(ctx, outbound.NotificationDelivery{Job: j, Destination: record.WebhookURL}); e != nil {
		t.Fatal(e)
	}
	claimed, e := st.ClaimNotification(ctx, now, time.Minute)
	if e != nil || claimed.Job.ID != j.ID {
		t.Fatal(claimed, e)
	}
	if e = st.ValidateNotificationDelivery(ctx, claimed, now); e != nil {
		t.Fatal(e)
	}
	forged := claimed
	forged.Job.Text = "different"
	if e = st.ValidateNotificationDelivery(ctx, forged, now); !errors.Is(e, domain.ErrRefConflict) {
		t.Fatal("altered queued text", e)
	}
	if e = st.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); e != nil {
		t.Fatal(e)
	}
	if e = st.ValidateNotificationDelivery(ctx, claimed, now); !errors.Is(e, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("late egress", e)
	}
	finish := claimed.Job
	finish.State = "delivered"
	finish.LeaseUntil = time.Time{}
	if e = st.FinishNotification(ctx, finish, now); !errors.Is(e, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("late receipt", e)
	}
	rows, e := st.ListNotifications(ctx, record.ID)
	if e != nil || len(rows) != 1 || rows[0].State != "running" {
		t.Fatal(rows, e)
	}
	// Pending unsupported work must not consume attempts, even in a global queue.
	j.ID = domain.NewID("evt_")
	if e = st.EnqueueNotification(ctx, outbound.NotificationDelivery{Job: j, Destination: record.WebhookURL}); e != nil {
		t.Fatal(e)
	}
	_, e = st.ClaimNotification(ctx, now, time.Minute)
	if e != nil && !errors.Is(e, domain.ErrNotFound) {
		t.Fatal(e)
	}
	rows, e = st.ListNotifications(ctx, record.ID)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range rows {
		if v.ID == j.ID && (v.Attempts != 0 || v.State != "pending") {
			t.Fatal("unsupported work claimed", v)
		}
	}
}
