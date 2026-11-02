//go:build postgres

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	secureadapter "github.com/wnsdy95/cxthub/backend/internal/adapters/webhookdelivery"
	"github.com/wnsdy95/cxthub/backend/internal/app/notificationworker"
	"github.com/wnsdy95/cxthub/backend/internal/app/systemmaintenance"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave41NotificationPGStore struct {
	*store.PostgresStore
	test        *testing.T
	actor       string
	calls       []string
	claims      []outbound.NotificationDelivery
	beforeGuard func(context.Context, outbound.NotificationDelivery) error
	guarded     atomic.Int64
}

func (storage *wave41NotificationPGStore) checkContext(ctx context.Context) {
	storage.test.Helper()
	actor, system := inbound.RepositoryActor(ctx)
	if actor != storage.actor || system || storage.InTransaction(ctx) {
		storage.test.Fatal("notification owner changed the caller actor or retained a transaction", actor, system)
	}
}

func (storage *wave41NotificationPGStore) ClaimNotification(ctx context.Context, now time.Time, lease time.Duration) (outbound.NotificationDelivery, error) {
	storage.checkContext(ctx)
	if lease != 2*time.Minute {
		storage.test.Fatal("notification owner changed the original claim lease", lease)
	}
	storage.calls = append(storage.calls, "claim")
	delivery, err := storage.PostgresStore.ClaimNotification(ctx, now, lease)
	if err == nil {
		storage.claims = append(storage.claims, delivery)
	}
	return delivery, err
}

func (storage *wave41NotificationPGStore) GetRepository(ctx context.Context, repository string) (domain.Repository, error) {
	storage.checkContext(ctx)
	storage.calls = append(storage.calls, "repository")
	return storage.PostgresStore.GetRepository(ctx, repository)
}

func (storage *wave41NotificationPGStore) ValidateNotificationDelivery(ctx context.Context, delivery outbound.NotificationDelivery, now time.Time) error {
	storage.checkContext(ctx)
	deadline, bounded := ctx.Deadline()
	if !bounded || time.Until(deadline) > 5*time.Second || now.IsZero() {
		storage.test.Fatal("egress guard lost the original send budget or current time")
	}
	storage.calls = append(storage.calls, "guard")
	if storage.beforeGuard != nil {
		if err := storage.beforeGuard(ctx, delivery); err != nil {
			storage.test.Fatal("prepare-to-egress fixture mutation failed", err)
		}
	}
	if err := storage.PostgresStore.ValidateNotificationDelivery(ctx, delivery, now); err != nil {
		return err
	}
	storage.guarded.Add(1)
	return nil
}

func (storage *wave41NotificationPGStore) FinishNotification(ctx context.Context, job domain.NotificationJob, now time.Time) error {
	storage.checkContext(ctx)
	storage.calls = append(storage.calls, "finish")
	return storage.PostgresStore.FinishNotification(ctx, job, now)
}

type wave41NextStorageUsage interface {
	ReconcileNextStorageUsage(context.Context) (bool, error)
}

type wave41MaintenancePGStore struct {
	*store.PostgresStore
	test         *testing.T
	actor        string
	runtimeCalls int
	storageCalls int
}

func (storage *wave41MaintenancePGStore) checkContext(ctx context.Context, budget time.Duration) {
	storage.test.Helper()
	actor, system := inbound.RepositoryActor(ctx)
	deadline, bounded := ctx.Deadline()
	if actor != storage.actor || system || !bounded || time.Until(deadline) > budget || storage.InTransaction(ctx) {
		storage.test.Fatal("maintenance owner changed the caller, transaction boundary or original budget", budget)
	}
}

func (storage *wave41MaintenancePGStore) PruneRuntimeState(ctx context.Context, now time.Time) error {
	storage.checkContext(ctx, 10*time.Second)
	storage.runtimeCalls++
	return storage.PostgresStore.PruneRuntimeState(ctx, now)
}

func (storage *wave41MaintenancePGStore) ReconcileNextStorageUsage(ctx context.Context) (bool, error) {
	storage.checkContext(ctx, 30*time.Second)
	storage.storageCalls++
	return storage.PostgresStore.ReconcileNextStorageUsage(ctx)
}

func wave41AuxiliaryPG(test *testing.T) (*store.PostgresStore, *pgx.Conn) {
	test.Helper()
	wave40IsolatedPG(test)
	_, postgres, _ := collaborationPG(test)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config, err := pgxpool.ParseConfig(os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal("invalid owned auxiliary test connection configuration")
	}
	connection, err := pgx.ConnectConfig(ctx, config.ConnConfig.Copy())
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, release := context.WithTimeout(context.Background(), 10*time.Second)
		defer release()
		if err := connection.Close(cleanup); err != nil {
			test.Error(err)
		}
	})
	return postgres, connection
}

func wave41AuxiliaryRepository(test *testing.T, postgres *store.PostgresStore, destination string) (domain.Repository, domain.ContentHash) {
	test.Helper()
	ctx := systemTestContext()
	now := time.Now().UTC()
	user := domain.User{ID: domain.NewID("user_"), Username: fmt.Sprintf("wave41%d", now.UnixNano()), Name: "Wave41", Email: "wave41@example.test"}
	if err := postgres.UpsertUser(ctx, user); err != nil {
		test.Fatal(err)
	}
	namespace := domain.Namespace{ID: domain.NewID("ns_"), Slug: user.Username, Kind: domain.NamespaceUser, UserID: user.ID, CreatedAt: now}
	if err := postgres.CreateNamespace(ctx, namespace); err != nil {
		test.Fatal(err)
	}
	record := domain.Repository{
		ID: domain.NewID("ws_"), Name: "Wave41", OwnerID: user.ID, OwnerUsername: user.Username,
		OwnerNamespaceID: namespace.ID, Slug: "auxiliary", CreatedAt: now, WebhookURL: destination,
	}
	if err := postgres.CreateRepository(ctx, record); err != nil {
		test.Fatal(err)
	}
	repo := domain.HashContent([]byte(record.ID))
	if _, err := postgres.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	return record, repo
}

func wave41EnqueueNotification(test *testing.T, postgres *store.PostgresStore, record domain.Repository, attempts int) domain.NotificationJob {
	test.Helper()
	now := time.Now().UTC()
	job := domain.NotificationJob{
		ID: domain.NewID("evt_"), RepositoryID: record.ID, Kind: "ref_updated", Text: "wave41 event metadata",
		State: "pending", Attempts: attempts, CreatedAt: now, UpdatedAt: now, NextAttempt: time.Unix(0, 0).UTC(),
	}
	if err := postgres.EnqueueNotification(systemTestContext(), outbound.NotificationDelivery{Job: job, Destination: record.WebhookURL}); err != nil {
		test.Fatal(err)
	}
	return job
}

func wave41NotificationState(test *testing.T, postgres *store.PostgresStore, record domain.Repository) domain.NotificationJob {
	test.Helper()
	jobs, err := postgres.ListNotifications(systemTestContext(), record.ID)
	if err != nil || len(jobs) != 1 {
		test.Fatal("expected one durable notification", len(jobs), err)
	}
	return jobs[0]
}

func wave41SecureSender(test testing.TB, client *http.Client) outbound.NotificationSender {
	test.Helper()
	sender, err := secureadapter.NewWithClient(client)
	if err != nil {
		test.Fatal(err)
	}
	return sender
}

func wave41NotificationOwner(test *testing.T, storage *wave41NotificationPGStore, metadata outbound.MetadataStore, client *http.Client) *notificationworker.Worker {
	test.Helper()
	worker, err := NewNotificationWorker(NotificationWorkerConfig{
		Profile: notificationworker.Complete, Metadata: metadata, Blobs: storage.PostgresStore,
		Repositories: storage, Sender: wave41SecureSender(test, client),
	})
	if err != nil || worker == nil {
		test.Fatal("public notification factory rejected complete PostgreSQL adapters", err)
	}
	return worker
}

func TestPGWave41NotificationFactoryDurableRetryRestartAndStableEventID(test *testing.T) {
	postgres, connection := wave41AuxiliaryPG(test)
	storage := &wave41NotificationPGStore{PostgresStore: postgres, test: test}
	var responseStatus atomic.Int64
	responseStatus.Store(http.StatusServiceUnavailable)
	var receiptMutex sync.Mutex
	var eventIDs []string
	receiver := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if storage.guarded.Load() != 1 {
			test.Error("HTTP egress occurred before successful current-delivery validation")
		}
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			test.Error(err)
		}
		if request.Method != http.MethodPost || request.URL.Path != "/events" || request.Header.Get("Content-Type") != "application/json" || len(body) != 1 || body["text"] != "wave41 event metadata" || request.Header.Get("Authorization") != "" {
			test.Error("notification request changed the original metadata-only HTTP contract")
		}
		receiptMutex.Lock()
		eventIDs = append(eventIDs, request.Header.Get("X-CXTHub-Event-ID"))
		receiptMutex.Unlock()
		response.Header().Set("Retry-After", "120")
		response.WriteHeader(int(responseStatus.Load()))
	}))
	test.Cleanup(receiver.Close)
	record, _ := wave41AuxiliaryRepository(test, postgres, receiver.URL+"/events")
	job := wave41EnqueueNotification(test, postgres, record, 0)
	storage.actor = record.OwnerID
	storage.beforeGuard = func(ctx context.Context, delivery outbound.NotificationDelivery) error {
		persisted := wave41NotificationState(test, postgres, record)
		if !reflect.DeepEqual(persisted, delivery.Job) || persisted.State != "running" || persisted.Attempts != 1 || persisted.Version != 1 {
			test.Fatal("egress did not observe a committed durable claim", persisted)
		}
		var due bool
		if err := connection.QueryRow(ctx, `SELECT lease_until>clock_timestamp() FROM notification_outbox WHERE id=$1`, job.ID).Scan(&due); err != nil || !due {
			test.Fatal("claimed lease is not live in PostgreSQL", due, err)
		}
		if _, err := postgres.ClaimNotification(ctx, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("another claimant acquired the active notification lease", err)
		}
		return nil
	}
	ctx := inbound.WithRepositoryActor(context.Background(), record.OwnerID)
	worker := wave41NotificationOwner(test, storage, storage, receiver.Client())
	if worked, err := worker.Process(ctx); err != nil || !worked {
		test.Fatal("native notification owner did not process the durable outbox", worked, err)
	}
	retrying := wave41NotificationState(test, postgres, record)
	if retrying.ID != job.ID || retrying.State != "retrying" || retrying.Reason != "http_retryable" || retrying.HTTPStatus != http.StatusServiceUnavailable || retrying.Attempts != 1 || retrying.Version != 1 || !retrying.LeaseUntil.IsZero() || retrying.NextAttempt.Sub(retrying.UpdatedAt) != 120*time.Second {
		test.Fatal("durable HTTP status, Retry-After or claim state changed", retrying)
	}
	if !slices.Equal(storage.calls, []string{"claim", "repository", "guard", "finish"}) {
		test.Fatal("notification transaction/egress/receipt order changed", storage.calls)
	}
	postgres.Close()
	restarted, err := store.NewPostgresStore(context.Background(), collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(restarted.Close)
	if persisted := wave41NotificationState(test, restarted, record); !reflect.DeepEqual(persisted, retrying) {
		test.Fatal("store restart lost the notification retry receipt", persisted)
	}
	storage.PostgresStore = restarted
	storage.beforeGuard = nil
	storage.calls = nil
	storage.guarded.Store(0)
	worker = wave41NotificationOwner(test, storage, storage, receiver.Client())
	if worked, err := worker.Process(ctx); err != nil || worked {
		test.Fatal("fresh owner ignored persisted Retry-After", worked, err)
	}
	if persisted := wave41NotificationState(test, restarted, record); !reflect.DeepEqual(persisted, retrying) {
		test.Fatal("idle recovery consumed an attempt or changed the retry receipt", persisted)
	}
	if err := restarted.RetryNotification(ctx, record.ID, job.ID, record.WebhookURL, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	responseStatus.Store(http.StatusNoContent)
	storage.calls = nil
	if worked, err := worker.Process(ctx); err != nil || !worked {
		test.Fatal("fresh public owner did not recover the explicitly retried event", worked, err)
	}
	delivered := wave41NotificationState(test, restarted, record)
	if delivered.ID != job.ID || delivered.State != "delivered" || delivered.Reason != "" || delivered.HTTPStatus != http.StatusNoContent || delivered.Version != retrying.Version+2 || delivered.Attempts != 1 || !delivered.LeaseUntil.IsZero() {
		test.Fatal("recovered delivery lost stable identity or retry/CAS fencing", delivered)
	}
	receiptMutex.Lock()
	received := slices.Clone(eventIDs)
	receiptMutex.Unlock()
	if !slices.Equal(received, []string{job.ID, job.ID}) {
		test.Fatal("restart changed the HTTP event ID or duplicated egress", received)
	}
	if err := restarted.RetryNotification(ctx, record.ID, job.ID, record.WebhookURL, time.Now().UTC()); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("delivered notification became replayable", err)
	}
	if worked, err := worker.Process(ctx); err != nil || worked {
		test.Fatal("delivered outbox remained claimable", worked, err)
	}
}

func TestPGWave41NotificationFactoryFencesStaleLeaseAndCurrentPolicy(test *testing.T) {
	postgres, connection := wave41AuxiliaryPG(test)
	var sends atomic.Int64
	receiver := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		sends.Add(1)
		response.WriteHeader(http.StatusNoContent)
	}))
	test.Cleanup(receiver.Close)
	for _, mode := range []string{"destination_revoked", "lease_reclaimed", "canceled_after_prepare", "identity_upgraded"} {
		test.Run(mode, func(test *testing.T) {
			record, repo := wave41AuxiliaryRepository(test, postgres, receiver.URL+"/events")
			wave41EnqueueNotification(test, postgres, record, 0)
			storage := &wave41NotificationPGStore{PostgresStore: postgres, test: test, actor: record.OwnerID}
			var metadata outbound.MetadataStore = storage
			if mode == "identity_upgraded" {
				metadata = struct {
					outbound.MetadataStore
					outbound.NotificationStore
					outbound.NotificationDeliveryGuard
				}{postgres, storage, storage}
				if !slices.Equal(ReadDocumentIdentities(metadata, postgres), []domain.DocumentIdentity{domain.DocumentIdentityLegacy}) {
					test.Fatal("legacy-only fixture accidentally advertised complete document readers")
				}
			}
			ctx, cancel := context.WithCancel(inbound.WithRepositoryActor(context.Background(), record.OwnerID))
			defer cancel()
			oracle := wave40PGContext(context.Background(), postgres)
			var reclaimed outbound.NotificationDelivery
			storage.beforeGuard = func(bound context.Context, delivery outbound.NotificationDelivery) error {
				switch mode {
				case "destination_revoked":
					record.WebhookURL = ""
					return postgres.CreateRepository(bound, record)
				case "identity_upgraded":
					return postgres.RequireDocumentIdentity(oracle, repo, domain.DocumentIdentityRootV1)
				case "lease_reclaimed":
					if _, err := connection.Exec(bound, `WITH expired AS (SELECT clock_timestamp()-interval '1 second' AS at) UPDATE notification_outbox SET lease_until=expired.at,payload=jsonb_set(payload,'{lease_until}',to_jsonb(expired.at)) FROM expired WHERE id=$1`, delivery.Job.ID); err != nil {
						return err
					}
					var err error
					reclaimed, err = postgres.ClaimNotification(oracle, time.Now().UTC(), 2*time.Minute)
					return err
				case "canceled_after_prepare":
					cancel()
				}
				return nil
			}
			worker := wave41NotificationOwner(test, storage, metadata, receiver.Client())
			expected := domain.ErrForbidden
			switch mode {
			case "identity_upgraded":
				expected = domain.ErrDocumentIdentityUpgradeRequired
			case "lease_reclaimed":
				expected = domain.ErrRefConflict
			case "canceled_after_prepare":
				expected = context.Canceled
			}
			if worked, err := worker.Process(ctx); !worked || !errors.Is(err, expected) {
				test.Fatal("native notification owner did not propagate the final real-store guard failure", worked, err)
			}
			if sends.Load() != 0 || storage.guarded.Load() != 0 || !slices.Equal(storage.calls, []string{"claim", "repository", "guard"}) || len(storage.claims) != 1 {
				test.Fatal("failed current-policy/lease/context guard sent or acknowledged an event", sends.Load(), storage.calls)
			}
			persisted := wave41NotificationState(test, postgres, record)
			if mode == "lease_reclaimed" {
				claimed := storage.claims[0]
				if reclaimed.Job.ID != claimed.Job.ID || reclaimed.Job.Version != claimed.Job.Version+1 || reclaimed.Job.Attempts != claimed.Job.Attempts+1 || !reflect.DeepEqual(persisted, reclaimed.Job) {
					test.Fatal("reclaim did not retain durable event identity and advance the fence", persisted, reclaimed)
				}
				claimed.Job.State = "delivered"
				claimed.Job.LeaseUntil = time.Time{}
				if err := postgres.FinishNotification(oracle, claimed.Job, time.Now().UTC()); !errors.Is(err, domain.ErrRefConflict) {
					test.Fatal("stale worker overwrote the reclaimed durable notification", err)
				}
				if current := wave41NotificationState(test, postgres, record); !reflect.DeepEqual(current, reclaimed.Job) {
					test.Fatal("stale receipt changed the winning durable claim", current)
				}
			} else if !reflect.DeepEqual(persisted, storage.claims[0].Job) {
				test.Fatal("guard rejection changed the original live claim", persisted)
			}
			if mode == "identity_upgraded" {
				if _, err := postgres.ClaimNotification(oracle, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("guard failure unexpectedly released a live claim", err)
				}
				wave41EnqueueNotification(test, postgres, record, 0)
				if worked, err := worker.Process(ctx); err != nil || worked {
					test.Fatal("unsupported pending identity consumed a durable attempt", worked, err)
				}
				jobs, err := postgres.ListNotifications(oracle, record.ID)
				if err != nil || len(jobs) != 2 {
					test.Fatal("identity fixture lost durable outbox rows", len(jobs), err)
				}
				for _, queued := range jobs {
					if queued.ID != persisted.ID && (queued.State != "pending" || queued.Attempts != 0 || queued.Version != 0) {
						test.Fatal("unsupported identity was claimed or acknowledged", queued)
					}
				}
			}
		})
	}
}

func TestPGWave41NotificationFactoryPreservesHTTPOutcomesErrorsAndCancellation(test *testing.T) {
	postgres, _ := wave41AuxiliaryPG(test)
	for _, outcome := range []struct {
		name       string
		status     int
		attempts   int
		retryAfter string
		state      string
		reason     string
		minimum    time.Duration
		maximum    time.Duration
	}{
		{name: "accepted", status: 204, state: "delivered"},
		{name: "redirect", status: 302, state: "attention", reason: "http_rejected"},
		{name: "rejected", status: 400, state: "attention", reason: "http_rejected"},
		{name: "timeout", status: 408, state: "retrying", reason: "http_retryable", minimum: 30 * time.Second, maximum: 30 * time.Second},
		{name: "too_early", status: 425, state: "retrying", reason: "http_retryable", minimum: 30 * time.Second, maximum: 30 * time.Second},
		{name: "rate_limited", status: 429, retryAfter: "180", state: "retrying", reason: "http_retryable", minimum: 180 * time.Second, maximum: 180 * time.Second},
		{name: "http_date", status: 503, state: "retrying", reason: "http_retryable", minimum: 9 * time.Minute, maximum: 10 * time.Minute},
		{name: "retry_cap", status: 503, retryAfter: "864000", state: "retrying", reason: "http_retryable", minimum: 24 * time.Hour, maximum: 24 * time.Hour},
		{name: "exhausted", status: 500, attempts: 7, state: "attention", reason: "attempts_exhausted"},
	} {
		test.Run(outcome.name, func(test *testing.T) {
			var sends atomic.Int64
			receiver := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				sends.Add(1)
				retryAfter := outcome.retryAfter
				if outcome.name == "http_date" {
					retryAfter = time.Now().UTC().Add(10 * time.Minute).Format(http.TimeFormat)
				}
				response.Header().Set("Retry-After", retryAfter)
				response.Header().Set("Location", "/must-not-follow")
				response.WriteHeader(outcome.status)
			}))
			defer receiver.Close()
			record, _ := wave41AuxiliaryRepository(test, postgres, receiver.URL)
			job := wave41EnqueueNotification(test, postgres, record, outcome.attempts)
			storage := &wave41NotificationPGStore{PostgresStore: postgres, test: test, actor: record.OwnerID}
			worker := wave41NotificationOwner(test, storage, storage, receiver.Client())
			if worked, err := worker.Process(inbound.WithRepositoryActor(context.Background(), record.OwnerID)); err != nil || !worked {
				test.Fatal("HTTP outcome escaped the native owner", worked, err)
			}
			persisted := wave41NotificationState(test, postgres, record)
			delay := persisted.NextAttempt.Sub(persisted.UpdatedAt)
			if persisted.ID != job.ID || persisted.State != outcome.state || persisted.Reason != outcome.reason || persisted.HTTPStatus != outcome.status || persisted.Attempts != outcome.attempts+1 || !persisted.LeaseUntil.IsZero() || delay < outcome.minimum || delay > outcome.maximum || sends.Load() != 1 {
				test.Fatal("original HTTP classification, Retry-After, redirect or attempt rule changed", persisted, sends.Load())
			}
		})
	}
	test.Run("invalid_destination", func(test *testing.T) {
		var sends atomic.Int64
		receiver := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			sends.Add(1)
			response.WriteHeader(http.StatusNoContent)
		}))
		defer receiver.Close()
		record, _ := wave41AuxiliaryRepository(test, postgres, "file:///wave41-invalid-destination")
		wave41EnqueueNotification(test, postgres, record, 0)
		storage := &wave41NotificationPGStore{PostgresStore: postgres, test: test, actor: record.OwnerID}
		worker := wave41NotificationOwner(test, storage, storage, receiver.Client())
		if worked, err := worker.Process(inbound.WithRepositoryActor(context.Background(), record.OwnerID)); err != nil || !worked {
			test.Fatal("invalid destination escaped the original terminal classification", worked, err)
		}
		persisted := wave41NotificationState(test, postgres, record)
		if persisted.State != "attention" || persisted.Reason != "invalid_destination" || persisted.HTTPStatus != 0 || persisted.Attempts != 1 || !persisted.LeaseUntil.IsZero() || storage.guarded.Load() != 0 || sends.Load() != 0 || !slices.Equal(storage.calls, []string{"claim", "repository", "finish"}) {
			test.Fatal("invalid destination was sent, guarded or durably classified incorrectly", persisted, storage.calls)
		}
	})
	test.Run("transport_failure", func(test *testing.T) {
		receiver := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			test.Error("closed local transport unexpectedly reached a receiver")
		}))
		client := receiver.Client()
		record, _ := wave41AuxiliaryRepository(test, postgres, receiver.URL)
		wave41EnqueueNotification(test, postgres, record, 0)
		receiver.Close()
		storage := &wave41NotificationPGStore{PostgresStore: postgres, test: test, actor: record.OwnerID}
		worker := wave41NotificationOwner(test, storage, storage, client)
		if worked, err := worker.Process(inbound.WithRepositoryActor(context.Background(), record.OwnerID)); err != nil || !worked {
			test.Fatal("transport error did not retain the original durable retry behavior", worked, err)
		}
		persisted := wave41NotificationState(test, postgres, record)
		if persisted.State != "retrying" || persisted.Reason != "transport_failed" || persisted.HTTPStatus != 0 || persisted.Attempts != 1 || persisted.NextAttempt.Sub(persisted.UpdatedAt) != 30*time.Second || storage.guarded.Load() != 1 {
			test.Fatal("transport failure was not guarded and durably retried", persisted)
		}
	})
	test.Run("canceled_before_claim", func(test *testing.T) {
		receiver := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			test.Error("canceled owner performed HTTP egress")
		}))
		defer receiver.Close()
		record, _ := wave41AuxiliaryRepository(test, postgres, receiver.URL)
		job := wave41EnqueueNotification(test, postgres, record, 0)
		storage := &wave41NotificationPGStore{PostgresStore: postgres, test: test, actor: record.OwnerID}
		worker := wave41NotificationOwner(test, storage, storage, receiver.Client())
		ctx, cancel := context.WithCancel(inbound.WithRepositoryActor(context.Background(), record.OwnerID))
		cancel()
		if worked, err := worker.Process(ctx); worked || !errors.Is(err, context.Canceled) {
			test.Fatal("pre-claim cancellation was swallowed", worked, err)
		}
		if persisted := wave41NotificationState(test, postgres, record); !reflect.DeepEqual(persisted, job) {
			test.Fatal("canceled owner changed a pending durable notification", persisted)
		}
	})
}

func TestPGWave41MaintenanceFactoryPrunesAndReconcilesDurableState(test *testing.T) {
	postgres, connection := wave41AuxiliaryPG(test)
	record, repo := wave41AuxiliaryRepository(test, postgres, "")
	ctx := inbound.WithRepositoryActor(context.Background(), record.OwnerID)
	var now time.Time
	if err := connection.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		test.Fatal(err)
	}
	expiredPairing := domain.DevicePairing{Code: "W41-EXPIRED", PollHash: domain.HashToken("wave41-expired-poll"), ExpiresAt: now.Add(-time.Hour)}
	livePairing := domain.DevicePairing{Code: "W41-LIVE", PollHash: domain.HashToken("wave41-live-poll"), ExpiresAt: now.Add(time.Hour)}
	for _, pairing := range []domain.DevicePairing{expiredPairing, livePairing} {
		if err := postgres.CreateDevicePairing(ctx, pairing); err != nil {
			test.Fatal(err)
		}
	}
	for _, allowance := range []struct {
		key string
		at  time.Time
	}{{"wave41-expired-allowance", now.Add(-time.Hour)}, {"wave41-live-allowance", now.Add(time.Hour)}} {
		if allowed, err := postgres.AllowRequest(ctx, allowance.key, 2, time.Minute, allowance.at); err != nil || !allowed {
			test.Fatal("runtime allowance fixture was not persisted", allowed, err)
		}
	}
	body := []byte("wave41 retained customer bytes remain unchanged by reconciliation")
	hash := domain.HashContent(body)
	if _, _, err := postgres.PutChunks(ctx, repo, map[domain.ContentHash][]byte{hash: body}); err != nil {
		test.Fatal(err)
	}
	readUsage := func(storage *store.PostgresStore) domain.StorageUsage {
		test.Helper()
		usage, err := storage.ReadStorageUsage(ctx, record.OwnerNamespaceID, now.Add(-time.Hour), time.Now().UTC().Add(time.Hour))
		if err != nil {
			test.Fatal(err)
		}
		return usage
	}
	before := readUsage(postgres)
	if before.CurrentBytes <= 0 || len(before.Entries) == 0 {
		test.Fatal("storage fixture lacks real retained-payload accounting", before)
	}
	transaction, err := connection.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer transaction.Rollback(context.Background())
	if tag, err := transaction.Exec(ctx, `UPDATE storage_usage_objects SET bytes=bytes+1 WHERE namespace_id=$1 AND object_key=$2`, record.OwnerNamespaceID, "blob:"+string(hash)); err != nil || tag.RowsAffected() != 1 {
		test.Fatal("projection-drift fixture did not target the owned payload", err)
	}
	if tag, err := transaction.Exec(ctx, `UPDATE storage_accounts SET current_bytes=current_bytes+1,reconciled_at=NULL,reconcile_after='-infinity' WHERE namespace_id=$1`, record.OwnerNamespaceID); err != nil || tag.RowsAffected() != 1 {
		test.Fatal("account-drift fixture did not target the owned namespace", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	storage := &wave41MaintenancePGStore{PostgresStore: postgres, test: test, actor: record.OwnerID}
	workers, err := NewMaintenanceWorkers(MaintenanceWorkersConfig{Profile: systemmaintenance.Complete, Repositories: storage})
	if err != nil || workers == nil || workers.Runtime == nil || workers.Storage == nil {
		test.Fatal("public maintenance factory did not assemble both complete PostgreSQL owners", err)
	}
	if err := workers.Runtime.Process(ctx); err != nil {
		test.Fatal("native runtime owner failed to prune actual expiring rows", err)
	}
	var expiredPairs, livePairs, expiredAllowances, liveAllowances int
	if err := connection.QueryRow(ctx, `SELECT (SELECT count(*) FROM device_pairings WHERE code=$1),(SELECT count(*) FROM device_pairings WHERE code=$2),(SELECT count(*) FROM request_allowances WHERE key=$3),(SELECT count(*) FROM request_allowances WHERE key=$4)`, expiredPairing.Code, livePairing.Code, "wave41-expired-allowance", "wave41-live-allowance").Scan(&expiredPairs, &livePairs, &expiredAllowances, &liveAllowances); err != nil || expiredPairs != 0 || livePairs != 1 || expiredAllowances != 0 || liveAllowances != 1 || storage.runtimeCalls != 1 {
		test.Fatal("runtime pruning did not durably separate expired and live state", expiredPairs, livePairs, expiredAllowances, liveAllowances, err)
	}
	if worked, err := workers.Storage.Process(ctx); err != nil || worked == 0 {
		test.Fatal("native storage owner did not reconcile a due namespace", worked, err)
	}
	fixed := readUsage(postgres)
	if fixed.CurrentBytes != before.CurrentBytes || len(fixed.Entries) != len(before.Entries)+1 || fixed.Entries[0].Reason != "reconcile" || fixed.Entries[0].DeltaBytes != -1 || fixed.Entries[0].BytesAfter != before.CurrentBytes || !slices.Equal(fixed.Entries[1:], before.Entries) || storage.storageCalls != 2 {
		test.Fatal("reconciliation did not preserve the corrective result and productive-then-idle batch", before, fixed, storage.storageCalls)
	}
	var currentBytes int64
	var reconciled, scheduled bool
	if err := connection.QueryRow(ctx, `SELECT current_bytes,reconciled_at IS NOT NULL,reconcile_after>clock_timestamp()+interval '23 hours' FROM storage_accounts WHERE namespace_id=$1`, record.OwnerNamespaceID).Scan(&currentBytes, &reconciled, &scheduled); err != nil || currentBytes != before.CurrentBytes || !reconciled || !scheduled {
		test.Fatal("storage reconciliation receipt or original durable schedule was not committed", currentBytes, reconciled, scheduled, err)
	}
	if worked, err := workers.Storage.Process(ctx); err != nil || worked != 0 || storage.storageCalls != 3 {
		test.Fatal("maintenance ignored the persisted next-reconciliation time", worked, err)
	}
	if again := readUsage(postgres); !reflect.DeepEqual(again.Entries, fixed.Entries) || again.CurrentBytes != fixed.CurrentBytes {
		test.Fatal("idle reconciliation rewrote prior accounting entries", again)
	}
	postgres.Close()
	restarted, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(restarted.Close)
	if persisted := readUsage(restarted); persisted.CurrentBytes != fixed.CurrentBytes || !slices.Equal(persisted.Entries, fixed.Entries) {
		test.Fatal("restart lost the corrective reconciliation", persisted)
	}
	if retained, err := restarted.GetChunk(ctx, repo, hash); err != nil || !bytes.Equal(retained, body) {
		test.Fatal("maintenance changed retained customer bytes", err)
	}
	if pairing, err := restarted.GetDevicePairing(ctx, livePairing.Code, livePairing.PollHash, now); err != nil || pairing.Code != livePairing.Code {
		test.Fatal("runtime maintenance changed live authorization state", err)
	}
	if _, err := restarted.GetUser(ctx, record.OwnerID); err != nil {
		test.Fatal("maintenance changed current identity state", err)
	}
}

func TestPGWave41AuxiliaryFactoriesValidateCompleteAndKeepUnsupportedCompatibility(test *testing.T) {
	postgres, _ := wave41AuxiliaryPG(test)
	receiver := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		test.Error("unsupported compatibility owner performed HTTP egress")
	}))
	test.Cleanup(receiver.Close)
	withoutJobs := struct{ outbound.MetadataStore }{postgres}
	withoutGuard := struct {
		outbound.MetadataStore
		outbound.NotificationStore
	}{postgres, postgres}
	for _, metadata := range []outbound.MetadataStore{withoutJobs, withoutGuard} {
		worker, err := NewNotificationWorker(NotificationWorkerConfig{
			Profile: notificationworker.Complete, Metadata: metadata, Blobs: postgres,
			Repositories: postgres, Sender: wave41SecureSender(test, receiver.Client()),
		})
		if err == nil || worker != nil {
			test.Fatal("complete notification factory accepted a missing durable capability", err)
		}
	}
	for _, missing := range []string{"sender", "repositories"} {
		config := NotificationWorkerConfig{
			Profile: notificationworker.Complete, Metadata: postgres, Blobs: postgres,
			Repositories: postgres, Sender: wave41SecureSender(test, receiver.Client()),
		}
		if missing == "sender" {
			config.Sender = nil
		} else {
			config.Repositories = nil
		}
		if worker, err := NewNotificationWorker(config); err == nil || worker != nil {
			test.Fatal("complete notification factory accepted missing delivery support", missing, err)
		}
	}
	compatibility, err := NewNotificationWorker(NotificationWorkerConfig{
		Profile: notificationworker.Compatibility, Metadata: withoutJobs, Blobs: postgres,
		Repositories: postgres, Sender: wave41SecureSender(test, receiver.Client()),
	})
	if err != nil || compatibility == nil {
		test.Fatal("unsupported compatibility notification assembly lost the original no-op", err)
	}
	if worked, err := compatibility.Process(context.Background()); err != nil || worked {
		test.Fatal("unsupported compatibility notification processing was not a no-op", worked, err)
	}
	withoutMaintenance := struct{ outbound.RepositoryStore }{postgres}
	runtimeOnly := struct {
		outbound.RepositoryStore
		outbound.RuntimeStore
	}{postgres, postgres}
	storageOnly := struct {
		outbound.RepositoryStore
		wave41NextStorageUsage
	}{postgres, postgres}
	for _, repositories := range []outbound.RepositoryStore{withoutMaintenance, runtimeOnly, storageOnly} {
		workers, err := NewMaintenanceWorkers(MaintenanceWorkersConfig{Profile: systemmaintenance.Complete, Repositories: repositories})
		if err == nil || workers != nil {
			test.Fatal("complete maintenance factory did not require both owners' support", err)
		}
	}
	workers, err := NewMaintenanceWorkers(MaintenanceWorkersConfig{Profile: systemmaintenance.Compatibility, Repositories: withoutMaintenance})
	if err != nil || workers == nil || workers.Runtime == nil || workers.Storage == nil {
		test.Fatal("unsupported compatibility maintenance lost nonnil disabled owners", err)
	}
	if err := workers.Runtime.Process(context.Background()); err != nil {
		test.Fatal("unsupported compatibility runtime owner was not a no-op", err)
	}
	if worked, err := workers.Storage.Process(context.Background()); err != nil || worked != 0 {
		test.Fatal("unsupported compatibility storage owner was not a no-op", worked, err)
	}
}
