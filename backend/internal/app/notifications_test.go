package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func notificationFixture(t *testing.T) (*Service, *store.FSStore, domain.Repository) {
	t.Helper()
	svc, st := newFsckSvc(t)
	repositoryRecord := domain.Repository{ID: domain.NewID("ws_"), Name: "test", OwnerID: "dev:notification-test", Visibility: domain.VisibilityPrivate, CreatedAt: time.Now().UTC(), WebhookURL: "https://example.test/hook"}
	if err := st.CreateRepository(context.Background(), repositoryRecord); err != nil {
		t.Fatal(err)
	}
	return svc, st, repositoryRecord
}
func TestNotificationRetryRestartAndCredentialIsolation(t *testing.T) {
	ctx := context.Background()
	svc, st, repositoryRecord := notificationFixture(t)
	var ids []string
	status := 503
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, r.Header.Get("X-CXTHub-Event-ID"))
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(status)
	}))
	defer receiver.Close()
	// Install a private-network test transport without changing production SSRF behavior.
	old := safeWebhookClient()
	webhookClient = receiver.Client()
	defer func() { webhookClient = old }()
	repositoryRecord.WebhookURL = receiver.URL + "/private-credential"
	if err := st.CreateRepository(ctx, repositoryRecord); err != nil {
		t.Fatal(err)
	}
	if err := enqueueRepositoryNotification(ctx, st, repositoryRecord, "secrets_updated", "metadata only"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProcessNotification(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, _ := st.ListNotifications(ctx, repositoryRecord.ID)
	if len(jobs) != 1 || jobs[0].State != "retrying" || jobs[0].HTTPStatus != 503 || jobs[0].NextAttempt.Before(time.Now().Add(115*time.Second)) {
		t.Fatalf("retry status: %+v", jobs)
	}
	// Simulate an operator retry after recovery; the same stored ID is reused.
	if err := st.RetryNotification(ctx, repositoryRecord.ID, jobs[0].ID, repositoryRecord.WebhookURL, time.Now()); err != nil {
		t.Fatal(err)
	}
	status = 204
	if _, err := svc.ProcessNotification(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, _ = st.ListNotifications(ctx, repositoryRecord.ID)
	if jobs[0].State != "delivered" || len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("delivery not stable: %+v %+v", jobs, ids)
	}
	if err := st.RetryNotification(ctx, repositoryRecord.ID, jobs[0].ID, repositoryRecord.WebhookURL, time.Now()); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("delivered event replayed")
	}
}
func TestNotificationClaimsAreExclusiveAndExpiredWorkersCannotFinish(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := store.NewFSStore(dir)
	peer := store.NewFSStore(dir)
	now := time.Now().UTC()
	j := domain.NotificationJob{ID: "event", RepositoryID: "repositoryRecord", State: "pending", CreatedAt: now, NextAttempt: now}
	if err := first.EnqueueNotification(ctx, outbound.NotificationDelivery{Job: j, Destination: "https://example.test/secret"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claims := make(chan outbound.NotificationDelivery, 2)
	for _, st := range []*store.FSStore{first, peer} {
		wg.Add(1)
		go func(st *store.FSStore) {
			defer wg.Done()
			d, err := st.ClaimNotification(ctx, now, time.Second)
			if err == nil {
				claims <- d
			} else if !errors.Is(err, domain.ErrNotFound) {
				t.Error(err)
			}
		}(st)
	}
	wg.Wait()
	close(claims)
	if len(claims) != 1 {
		t.Fatalf("duplicate claims: %d", len(claims))
	}
	old := <-claims
	fresh, err := peer.ClaimNotification(ctx, now.Add(2*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	old.Job.State = "delivered"
	if err = first.FinishNotification(ctx, old.Job, now.Add(2*time.Second)); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatal("stale worker completed")
	}
	fresh.Job.State = "delivered"
	if err = peer.FinishNotification(ctx, fresh.Job, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	restarted := store.NewFSStore(dir)
	jobs, err := restarted.ListNotifications(ctx, "repositoryRecord")
	if err != nil || len(jobs) != 1 || jobs[0].State != "delivered" {
		t.Fatalf("restart lost job: %+v %v", jobs, err)
	}
}
func TestNotificationChangedDestinationRequiresExplicitRetry(t *testing.T) {
	ctx := context.Background()
	svc, st, repositoryRecord := notificationFixture(t)
	if err := enqueueRepositoryNotification(ctx, st, repositoryRecord, "ref_updated", "safe metadata"); err != nil {
		t.Fatal(err)
	}
	repositoryRecord.WebhookURL = "https://example.test/replacement"
	if err := st.CreateRepository(ctx, repositoryRecord); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProcessNotification(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, _ := st.ListNotifications(ctx, repositoryRecord.ID)
	if jobs[0].State != "attention" || jobs[0].Reason != "destination_changed" {
		t.Fatalf("sent stale destination: %+v", jobs)
	}
}

func TestNotificationHTTPOutcomeAndExhaustion(t *testing.T) {
	for _, tc := range []struct {
		code, prior   int
		state, reason string
	}{
		{204, 0, "delivered", ""}, {302, 0, "attention", "http_rejected"}, {400, 0, "attention", "http_rejected"},
		{408, 0, "retrying", "http_retryable"}, {429, 0, "retrying", "http_retryable"}, {500, 7, "attention", "attempts_exhausted"},
	} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			ctx := context.Background()
			svc, st, repositoryRecord := notificationFixture(t)
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://example.test/unexpected")
				w.WriteHeader(tc.code)
			}))
			defer receiver.Close()
			// Use the test transport while retaining the production redirect policy.
			old := safeWebhookClient()
			client := receiver.Client()
			client.CheckRedirect = old.CheckRedirect
			webhookClient = client
			defer func() { webhookClient = old }()
			repositoryRecord.WebhookURL = receiver.URL
			if err := st.CreateRepository(ctx, repositoryRecord); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			j := domain.NotificationJob{ID: domain.NewID("evt_"), RepositoryID: repositoryRecord.ID, State: "pending", Text: "metadata", Attempts: tc.prior, CreatedAt: now, NextAttempt: now}
			if err := st.EnqueueNotification(ctx, outbound.NotificationDelivery{Job: j, Destination: repositoryRecord.WebhookURL}); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.ProcessNotification(ctx); err != nil {
				t.Fatal(err)
			}
			jobs, _ := st.ListNotifications(ctx, repositoryRecord.ID)
			if len(jobs) != 1 || jobs[0].State != tc.state || jobs[0].Reason != tc.reason {
				t.Fatalf("unexpected outcome: %+v", jobs)
			}
		})
	}
}
