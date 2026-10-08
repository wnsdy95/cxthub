//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestWorkerPGNotificationGuardLeaseWait(t *testing.T) {
	for _, mode := range []string{"expires-under-policy-lock", "live-after-policy-lock", "cancel-under-policy-lock"} {
		t.Run(mode, func(t *testing.T) {
			st, ctx := chunkReusePG(t)
			user := domain.User{ID: domain.NewID("user_"), Username: fmt.Sprintf("lease%d", time.Now().UnixNano()), Email: "synthetic@example.test"}
			if err := st.UpsertUser(ctx, user); err != nil {
				t.Fatal(err)
			}
			record := domain.Repository{ID: domain.NewID("ws_"), Name: "Synthetic lease wait", OwnerID: user.ID, OwnerUsername: user.Username, Slug: "lease", CreatedAt: time.Now().UTC(), WebhookURL: "https://example.test/unused"}
			if err := st.CreateRepository(ctx, record); err != nil {
				t.Fatal(err)
			}
			repo := domain.HashContent([]byte(record.ID))
			if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			job := domain.NotificationJob{ID: domain.NewID("evt_"), RepositoryID: record.ID, Kind: "synthetic", Text: "no egress", State: "pending", CreatedAt: now, UpdatedAt: now, NextAttempt: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}
			if err := st.EnqueueNotification(ctx, outbound.NotificationDelivery{Job: job, Destination: record.WebhookURL}); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := st.pool.Exec(ctx, `DELETE FROM notification_outbox WHERE id=$1`, job.ID); err != nil {
					t.Error(err)
				}
			}()
			lease := 5 * time.Second
			if mode == "expires-under-policy-lock" {
				lease = 400 * time.Millisecond
			}
			claim, err := st.ClaimNotification(ctx, now, lease)
			if err != nil || claim.Job.ID != job.ID {
				t.Fatal("wrong synthetic claim", err)
			}
			// Compare persisted images, not pgx and JSON time representations.
			before, err := st.ListNotifications(ctx, record.ID)
			if err != nil || len(before) != 1 || before[0].ID != claim.Job.ID {
				t.Fatal("read persisted claim baseline", before, err)
			}
			var beforeRow string
			if err := st.pool.QueryRow(ctx, `SELECT row_to_json(n)::text FROM notification_outbox n WHERE id=$1`, claim.Job.ID).Scan(&beforeRow); err != nil {
				t.Fatal(err)
			}
			if err := st.ValidateNotificationDelivery(ctx, claim, now); err != nil {
				t.Fatal("unexpired positive control", err)
			}
			blocker, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			var blockerPID int
			if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid() FROM repos WHERE id=$1 FOR UPDATE`, repo).Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			guard, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			entered := time.Now().UTC()
			callerTime := entered
			if mode == "live-after-policy-lock" {
				// PostgreSQL has always used its own authoritative clock, not
				// a caller's future timestamp; keep that adapter contract.
				callerTime = claim.Job.LeaseUntil.Add(time.Second)
			}
			go func() { done <- st.ValidateNotificationDelivery(guard, claim, callerTime) }()
			for {
				var blocked bool
				if err := st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) AND query LIKE 'SELECT required_doc_identity%')`, blockerPID).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case err := <-done:
					t.Fatal("guard bypassed held policy row", err)
				case <-guard.Done():
					t.Fatal("guard did not reach policy wait", guard.Err())
				case <-time.After(time.Millisecond):
				}
			}
			var dbNow time.Time
			if err := st.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil || !dbNow.Before(claim.Job.LeaseUntil) {
				t.Fatal("guard must reach policy wait before expiry", err)
			}
			if mode == "expires-under-policy-lock" {
				timer := time.NewTimer(claim.Job.LeaseUntil.Sub(dbNow) + 40*time.Millisecond)
				defer timer.Stop()
				select {
				case err := <-done:
					t.Fatal("guard escaped policy lock", err)
				case <-timer.C:
				case <-guard.Done():
					t.Fatal(guard.Err())
				}
			} else if mode == "cancel-under-policy-lock" {
				cancel()
			}
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("guard did not finish after releasing policy lock")
			}
			t.Logf("mode=%s elapsed=%s err=%v", mode, time.Since(entered), err)
			switch mode {
			case "expires-under-policy-lock":
				if !errors.Is(err, domain.ErrRefConflict) {
					t.Error("expired claim admitted after retained policy lock wait", err)
				}
			case "live-after-policy-lock":
				if err != nil {
					t.Error("valid claim rejected", err)
				}
			case "cancel-under-policy-lock":
				if !errors.Is(err, context.Canceled) {
					t.Error("cancellation lost", err)
				}
			}
			jobs, err := st.ListNotifications(ctx, record.ID)
			if err != nil || len(jobs) != 1 || !reflect.DeepEqual(jobs, before) {
				t.Fatal("guard changed durable claim", jobs, err)
			}
			var afterRow string
			if err := st.pool.QueryRow(ctx, `SELECT row_to_json(n)::text FROM notification_outbox n WHERE id=$1`, claim.Job.ID).Scan(&afterRow); err != nil {
				t.Fatal(err)
			}
			if afterRow != beforeRow {
				t.Fatal("guard changed durable notification row", beforeRow, afterRow)
			}
		})
	}
}
