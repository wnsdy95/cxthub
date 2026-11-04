package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/notificationworker"
	"github.com/wnsdy95/cxthub/backend/internal/app/systemmaintenance"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave41NotificationSender struct {
	prepare func(context.Context, outbound.NotificationDelivery) (outbound.NotificationRequest, error)
}

func (sender *wave41NotificationSender) Prepare(ctx context.Context, delivery outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
	return sender.prepare(ctx, delivery)
}

type wave41NotificationRequest struct {
	send func(context.Context) (outbound.NotificationResponse, error)
}

func (request *wave41NotificationRequest) Send(ctx context.Context) (outbound.NotificationResponse, error) {
	return request.send(ctx)
}

type wave41NotificationStore struct {
	*store.FSStore
	calls    []string
	seen     context.Context
	guardCtx context.Context
	claimAt  time.Time
	guardAt  time.Time
	lease    time.Duration
	claimed  outbound.NotificationDelivery
	guarded  outbound.NotificationDelivery
}

func (storage *wave41NotificationStore) ClaimNotification(ctx context.Context, now time.Time, lease time.Duration) (outbound.NotificationDelivery, error) {
	storage.calls = append(storage.calls, "claim")
	storage.seen, storage.claimAt, storage.lease = ctx, now, lease
	delivery, err := storage.FSStore.ClaimNotification(ctx, now, lease)
	storage.claimed = delivery
	return delivery, err
}

func (storage *wave41NotificationStore) GetRepository(ctx context.Context, repository string) (domain.Repository, error) {
	storage.calls = append(storage.calls, "repository")
	return storage.FSStore.GetRepository(ctx, repository)
}

func (storage *wave41NotificationStore) ValidateNotificationDelivery(ctx context.Context, delivery outbound.NotificationDelivery, now time.Time) error {
	storage.calls = append(storage.calls, "guard")
	storage.guardCtx, storage.guarded, storage.guardAt = ctx, delivery, now
	return storage.FSStore.ValidateNotificationDelivery(ctx, delivery, now)
}

func (storage *wave41NotificationStore) FinishNotification(ctx context.Context, job domain.NotificationJob, now time.Time) error {
	storage.calls = append(storage.calls, "finish")
	return storage.FSStore.FinishNotification(ctx, job, now)
}

func wave41NotificationConfig(test *testing.T, profile notificationworker.Profile) (NotificationWorkerConfig, *wave41NotificationStore, domain.Repository) {
	test.Helper()
	_, storage, repository := notificationFixture(test)
	observed := &wave41NotificationStore{FSStore: storage}
	sender := &wave41NotificationSender{prepare: func(context.Context, outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
		test.Fatal("unexpected notification preparation")
		return nil, domain.ErrForbidden
	}}
	return NotificationWorkerConfig{Profile: profile, Metadata: observed, Blobs: storage, Repositories: observed, Sender: sender}, observed, repository
}

func TestWave41NotificationFactoryProfilesAndMandatoryAdapters(test *testing.T) {
	for _, profile := range []notificationworker.Profile{notificationworker.Compatibility, notificationworker.Complete} {
		test.Run(string(profile), func(test *testing.T) {
			config, storage, _ := wave41NotificationConfig(test, profile)
			worker, err := NewNotificationWorker(config)
			if err != nil || worker == nil {
				test.Fatal("FS notification capabilities rejected", err)
			}
			if len(storage.calls) != 0 {
				test.Fatal("constructor executed notification work", storage.calls)
			}
			for _, unknown := range []notificationworker.Profile{"", "future-profile"} {
				invalid := config
				invalid.Profile = unknown
				if result, err := NewNotificationWorker(invalid); result != nil || !errors.Is(err, domain.ErrValidation) {
					test.Fatal("unknown notification profile accepted", unknown, err)
				}
			}
			if profile != notificationworker.Complete {
				return
			}
			for _, field := range []string{"Metadata", "Blobs", "Repositories", "Sender"} {
				for _, typed := range []bool{false, true} {
					invalid := config
					value := reflect.ValueOf(&invalid).Elem().FieldByName(field)
					value.Set(reflect.Zero(value.Type()))
					if typed {
						missing := any((*store.FSStore)(nil))
						if field == "Sender" {
							missing = (*wave41NotificationSender)(nil)
						}
						value.Set(reflect.ValueOf(missing))
						if value.IsNil() || !value.Elem().IsNil() {
							test.Fatal("typed-nil fixture is not an interface containing a nil pointer", field)
						}
					}
					if result, err := NewNotificationWorker(invalid); result != nil || err == nil {
						test.Fatal("complete notification accepted missing adapter", field, typed, err)
					}
				}
			}
			if len(storage.calls) != 0 {
				test.Fatal("rejected constructor executed notification work", storage.calls)
			}
		})
	}
}

func TestWave41CompatibilityNotificationAllowsMetadataOnlyDeclaration(test *testing.T) {
	config, storage, repository := wave41NotificationConfig(test, notificationworker.Compatibility)
	config.Blobs = nil
	prepared := false
	config.Sender = &wave41NotificationSender{prepare: func(bound context.Context, _ outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
		actor, system := inbound.RepositoryActor(bound)
		if actor != "caller" || system || !reflect.DeepEqual(inbound.DocumentIdentities(bound), []domain.DocumentIdentity{domain.DocumentIdentityLegacy}) {
			test.Fatal("metadata-only worker changed actor or declared absent root readers", actor, system, inbound.DocumentIdentities(bound))
		}
		prepared = true
		return &wave41NotificationRequest{send: func(context.Context) (outbound.NotificationResponse, error) {
			return outbound.NotificationResponse{HTTPStatus: 204}, nil
		}}, nil
	}}
	worker, err := NewNotificationWorker(config)
	if err != nil || worker == nil {
		test.Fatal("compatibility rejected original metadata-only support", err)
	}
	if err := enqueueRepositoryNotification(systemTestContext(), storage, repository, "synthetic", "metadata only"); err != nil {
		test.Fatal(err)
	}
	if worked, err := worker.Process(inbound.WithRepositoryActor(context.Background(), "caller")); !worked || err != nil || !prepared {
		test.Fatal("metadata-only notification failed", worked, err, prepared)
	}
}

func TestWave41CompleteNotificationRequiresJobsAndDeliveryGuard(test *testing.T) {
	for _, missing := range []string{"jobs", "guard"} {
		test.Run(missing, func(test *testing.T) {
			config, storage, _ := wave41NotificationConfig(test, notificationworker.Complete)
			switch missing {
			case "jobs":
				config.Metadata = struct {
					outbound.MetadataStore
					outbound.NotificationDeliveryGuard
				}{MetadataStore: storage, NotificationDeliveryGuard: storage}
			case "guard":
				config.Metadata = struct {
					outbound.MetadataStore
					outbound.NotificationStore
				}{MetadataStore: storage, NotificationStore: storage}
			}
			if worker, err := NewNotificationWorker(config); worker != nil || err == nil {
				test.Fatal("complete notification fabricated a missing capability", missing, err)
			}
			if len(storage.calls) != 0 {
				test.Fatal("capability validation claimed work", storage.calls)
			}
		})
	}
}

func TestWave41ServiceNotificationRequiresExplicitSenderBeforeClaim(test *testing.T) {
	for _, scenario := range []string{"unconfigured", "nil", "typed-nil"} {
		test.Run(scenario, func(test *testing.T) {
			service, storage, repository := notificationFixture(test)
			observed := &wave41NotificationStore{FSStore: storage}
			service.meta = observed
			ctx := systemTestContext()
			if err := enqueueRepositoryNotification(ctx, storage, repository, "synthetic", "metadata only"); err != nil {
				test.Fatal(err)
			}
			before, err := storage.ListNotifications(ctx, repository.ID)
			if err != nil || len(before) != 1 {
				test.Fatal(before, err)
			}
			if scenario != "unconfigured" {
				var sender outbound.NotificationSender
				if scenario == "typed-nil" {
					sender = (*wave41NotificationSender)(nil)
				}
				if err := service.ConfigureNotificationSender(sender); err == nil {
					test.Fatal("missing explicit sender accepted", scenario)
				}
			}
			if worked, err := service.ProcessNotification(ctx); worked || !errors.Is(err, domain.ErrCatalogUnsupported) {
				test.Fatal("missing sender did not fail before claim", worked, err)
			}
			if len(observed.calls) != 0 {
				test.Fatal("missing sender touched notification lease", observed.calls)
			}
			after, err := storage.ListNotifications(ctx, repository.ID)
			if err != nil || !reflect.DeepEqual(after, before) || after[0].Attempts != 0 || !after[0].LeaseUntil.IsZero() {
				test.Fatal("missing sender mutated pending notification", after, err)
			}
			prepared, sent := 0, 0
			sender := &wave41NotificationSender{prepare: func(context.Context, outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
				prepared++
				return &wave41NotificationRequest{send: func(context.Context) (outbound.NotificationResponse, error) {
					sent++
					return outbound.NotificationResponse{HTTPStatus: 204}, nil
				}}, nil
			}}
			if err := service.ConfigureNotificationSender(sender); err != nil || prepared != 0 || sent != 0 || len(observed.calls) != 0 {
				test.Fatal("explicit sender configuration executed work", prepared, sent, observed.calls, err)
			}
			if worked, err := service.ProcessNotification(ctx); !worked || err != nil || prepared != 1 || sent != 1 {
				test.Fatal("explicit sender did not enable legacy notification", worked, prepared, sent, err)
			}
			after, err = storage.ListNotifications(ctx, repository.ID)
			if err != nil || len(after) != 1 || after[0].ID != before[0].ID || after[0].State != "delivered" || after[0].HTTPStatus != 204 || after[0].Attempts != 1 || !after[0].LeaseUntil.IsZero() {
				test.Fatal("explicit sender did not retain the unclaimed event", after, err)
			}
		})
	}
}

func TestWave41CompatibilityNotificationUnsupportedJobsOrRepositoriesRemainNoop(test *testing.T) {
	for _, missing := range []string{"jobs", "repositories"} {
		test.Run(missing, func(test *testing.T) {
			service, storage, repository := notificationFixture(test)
			observed := &wave41NotificationStore{FSStore: storage}
			config := NotificationWorkerConfig{Profile: notificationworker.Compatibility, Metadata: observed, Blobs: storage, Repositories: observed}
			service.meta = observed
			if missing == "jobs" {
				config.Metadata = struct{ outbound.MetadataStore }{MetadataStore: storage}
				service.meta = config.Metadata
			} else {
				config.Repositories = nil
				service.repositories = nil
			}
			ctx := systemTestContext()
			if err := enqueueRepositoryNotification(ctx, storage, repository, "synthetic", "metadata only"); err != nil {
				test.Fatal(err)
			}
			worker, err := NewNotificationWorker(config)
			if err != nil || worker == nil {
				test.Fatal("unsupported compatibility constructor lost no-op", missing, err)
			}
			for _, process := range []func(context.Context) (bool, error){worker.Process, service.ProcessNotification} {
				if worked, err := process(ctx); worked || err != nil {
					test.Fatal("unsupported compatibility execution lost no-op", missing, worked, err)
				}
			}
			if len(observed.calls) != 0 {
				test.Fatal("unsupported no-op claimed notification", observed.calls)
			}
			jobs, err := storage.ListNotifications(ctx, repository.ID)
			if err != nil || len(jobs) != 1 || jobs[0].State != "pending" || jobs[0].Attempts != 0 || !jobs[0].LeaseUntil.IsZero() {
				test.Fatal("unsupported no-op changed pending notification", jobs, err)
			}
		})
	}
}

func TestWave41NativeNotificationReplacesSupportDeclarationWithoutChangingActor(test *testing.T) {
	for _, rootRuntime := range []bool{false, true} {
		for _, actor := range []string{"", "user", "system"} {
			for _, requested := range []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1, "future-unsupported"} {
				config, storage, _ := wave41NotificationConfig(test, notificationworker.Complete)
				if !rootRuntime {
					config.Blobs = struct{ outbound.BlobStore }{BlobStore: storage.FSStore}
				}
				worker, err := NewNotificationWorker(config)
				if err != nil || worker == nil {
					test.Fatal("notification construction depended on document publication support", err)
				}
				ctx := context.Background()
				if actor == "user" {
					ctx = inbound.WithRepositoryActor(ctx, "synthetic")
				} else if actor == "system" {
					ctx = inbound.WithSystemActor(ctx)
				}
				user, system := inbound.RepositoryActor(ctx)
				caller := []domain.DocumentIdentity{requested}
				ctx = inbound.WithDocumentIdentities(ctx, caller)
				ctx = outbound.WithDocumentIdentityCompatibility(ctx, caller, caller)
				if worked, err := worker.Process(ctx); worked || err != nil {
					test.Fatal("empty notification queue did work", rootRuntime, actor, requested, worked, err)
				}
				if storage.seen == nil {
					test.Fatal("notification worker never reached its native store")
				}
				if actualUser, actualSystem := inbound.RepositoryActor(storage.seen); actualUser != user || actualSystem != system {
					test.Fatal("notification worker changed caller actor", actor, actualUser, actualSystem)
				}
				want := ReadDocumentIdentities(config.Metadata, config.Blobs)
				if hasDocumentIdentity(want, domain.DocumentIdentityRootV1) != rootRuntime || !reflect.DeepEqual(inbound.DocumentIdentities(storage.seen), want) {
					test.Fatal("worker used caller request instead of runtime support", rootRuntime, requested, inbound.DocumentIdentities(storage.seen), want)
				}
				wantErr := error(domain.ErrDocumentIdentityUpgradeRequired)
				if rootRuntime {
					wantErr = nil
				}
				if err := outbound.CheckDocumentIdentityCompatibility(storage.seen, domain.DocumentIdentityRootV1); !errors.Is(err, wantErr) {
					test.Fatal("worker compatibility declaration disagrees with runtime", err)
				}
				canceled, cancel := context.WithCancel(storage.seen)
				cancel()
				if err := outbound.CheckDocumentIdentityCompatibility(canceled, domain.DocumentIdentityRootV1); !errors.Is(err, context.Canceled) {
					test.Fatal("worker declaration lost cancellation", err)
				}
				if err := outbound.CheckDocumentIdentityCompatibility(context.WithoutCancel(canceled), domain.DocumentIdentityRootV1); !errors.Is(err, wantErr) {
					test.Fatal("cleanup lost worker support declaration", err)
				}
				if !reflect.DeepEqual(inbound.DocumentIdentities(ctx), caller) {
					test.Fatal("worker mutated caller declaration")
				}
			}
		}
	}
}

func TestWave41NativeNotificationPreparesThenGuardsImmediatelyBeforeSend(test *testing.T) {
	for _, revoked := range []bool{false, true} {
		config, storage, repository := wave41NotificationConfig(test, notificationworker.Complete)
		ctx := inbound.WithRepositoryActor(context.Background(), "synthetic")
		if err := enqueueRepositoryNotification(ctx, storage.FSStore, repository, "synthetic", "queued metadata"); err != nil {
			test.Fatal(err)
		}
		config.Sender = &wave41NotificationSender{prepare: func(prepareCtx context.Context, delivery outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
			storage.calls = append(storage.calls, "prepare")
			if user, system := inbound.RepositoryActor(prepareCtx); user != "synthetic" || system {
				test.Fatal("preparation changed caller actor", user, system)
			}
			if !reflect.DeepEqual(delivery, storage.claimed) {
				test.Fatal("preparation did not receive the exact claim", delivery)
			}
			if revoked {
				repository.WebhookURL = ""
				if err := storage.FSStore.CreateRepository(prepareCtx, repository); err != nil {
					test.Fatal(err)
				}
			}
			return &wave41NotificationRequest{send: func(sendCtx context.Context) (outbound.NotificationResponse, error) {
				storage.calls = append(storage.calls, "send")
				if sendCtx != storage.guardCtx {
					test.Fatal("guard and egress used different contexts")
				}
				if user, system := inbound.RepositoryActor(sendCtx); user != "synthetic" || system {
					test.Fatal("egress changed caller actor", user, system)
				}
				return outbound.NotificationResponse{HTTPStatus: 204}, nil
			}}, nil
		}}
		worker, err := NewNotificationWorker(config)
		if err != nil || worker == nil || len(storage.calls) != 0 {
			test.Fatal("constructor executed queued notification", storage.calls, err)
		}
		worked, err := worker.Process(ctx)
		wantErr := error(nil)
		wantCalls := []string{"claim", "repository", "prepare", "guard", "send", "finish"}
		if revoked {
			wantErr = domain.ErrForbidden
			wantCalls = []string{"claim", "repository", "prepare", "guard"}
		}
		if !worked || !errors.Is(err, wantErr) || !reflect.DeepEqual(storage.calls, wantCalls) {
			test.Fatal("prepare/guard/send order changed", revoked, worked, storage.calls, err)
		}
		if storage.lease != 2*time.Minute || storage.guardAt.Before(storage.claimAt) || !reflect.DeepEqual(storage.claimed, storage.guarded) {
			test.Fatal("guard lost exact claim, lease or time", storage.lease, storage.claimAt, storage.guardAt)
		}
		deadline, bounded := storage.guardCtx.Deadline()
		if !bounded || deadline.After(storage.guardAt.Add(5*time.Second)) {
			test.Fatal("egress lost original five-second deadline", deadline)
		}
		jobs, err := storage.FSStore.ListNotifications(ctx, repository.ID)
		if err != nil || len(jobs) != 1 || jobs[0].Attempts != 1 {
			test.Fatal(jobs, err)
		}
		if revoked {
			if jobs[0].State != "running" || jobs[0].HTTPStatus != 0 || jobs[0].LeaseUntil.IsZero() {
				test.Fatal("revoked delivery published a receipt or released its claim", jobs)
			}
		} else if jobs[0].State != "delivered" || jobs[0].HTTPStatus != 204 || !jobs[0].LeaseUntil.IsZero() {
			test.Fatal("successful delivery lost original receipt", jobs)
		}
	}
}

func TestWave41NativeNotificationIdentityTimingKeepsClaimAndReceiptFences(test *testing.T) {
	for _, rootRuntime := range []bool{false, true} {
		for _, when := range []string{"before-claim", "before-egress", "after-send", "legacy"} {
			config, storage, repository := wave41NotificationConfig(test, notificationworker.Complete)
			if !rootRuntime {
				config.Blobs = struct{ outbound.BlobStore }{BlobStore: storage.FSStore}
			}
			ctx := inbound.WithRepositoryActor(context.Background(), "synthetic")
			repo := domain.HashContent([]byte(test.Name() + when))
			if _, err := storage.FSStore.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: repository.ID}); err != nil {
				test.Fatal(err)
			}
			if err := enqueueRepositoryNotification(ctx, storage.FSStore, repository, "synthetic", "queued metadata"); err != nil {
				test.Fatal(err)
			}
			upgrade := func() {
				test.Helper()
				if err := storage.FSStore.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
					test.Fatal(err)
				}
			}
			if when == "before-claim" {
				upgrade()
			}
			sends := 0
			config.Sender = &wave41NotificationSender{prepare: func(context.Context, outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
				if when == "before-egress" {
					upgrade()
				}
				return &wave41NotificationRequest{send: func(context.Context) (outbound.NotificationResponse, error) {
					sends++
					if when == "after-send" {
						upgrade()
					}
					return outbound.NotificationResponse{HTTPStatus: 204}, nil
				}}, nil
			}}
			worker, err := NewNotificationWorker(config)
			if err != nil || worker == nil {
				test.Fatal(err)
			}
			roots := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
			ctx = inbound.WithDocumentIdentities(ctx, roots)
			ctx = outbound.WithDocumentIdentityCompatibility(ctx, roots, roots)
			worked, err := worker.Process(ctx)
			if when == "before-claim" && !rootRuntime {
				if worked || err != nil {
					test.Fatal("unsupported claim was not skipped", worked, err)
				}
			} else if when == "legacy" || rootRuntime {
				if !worked || err != nil {
					test.Fatal("supported delivery failed", when, rootRuntime, worked, err)
				}
			} else if !worked || !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
				test.Fatal("notification crossed identity timing fence", when, worked, err)
			}
			if user, system := inbound.RepositoryActor(storage.seen); user != "synthetic" || system {
				test.Fatal("notification timing path changed actor", user, system)
			}
			wantSends := 0
			if rootRuntime || when == "after-send" || when == "legacy" {
				wantSends = 1
			}
			if sends != wantSends {
				test.Fatal("notification egress crossed identity fence", when, rootRuntime, sends)
			}
			jobs, err := storage.FSStore.ListNotifications(ctx, repository.ID)
			if err != nil || len(jobs) != 1 {
				test.Fatal(jobs, err)
			}
			if when == "legacy" || rootRuntime {
				if jobs[0].State != "delivered" || jobs[0].HTTPStatus != 204 {
					test.Fatal("supported delivery lost receipt", jobs)
				}
			} else if jobs[0].State == "delivered" || jobs[0].HTTPStatus != 0 {
				test.Fatal("receipt crossed identity opt-in", jobs)
			}
			wantAttempts := 1
			if when == "before-claim" && !rootRuntime {
				wantAttempts = 0
			}
			if jobs[0].Attempts != wantAttempts {
				test.Fatal("identity timing changed claim attempts", when, rootRuntime, jobs)
			}
		}
	}
}

func TestWave41NativeNotificationResponsePreservesRetryAfterAndHTTPStatus(test *testing.T) {
	for _, scenario := range []struct {
		delay time.Duration
		want  time.Duration
	}{
		{delay: 120 * time.Second, want: 120 * time.Second},
		{delay: 30 * time.Hour, want: 24 * time.Hour},
		{delay: 0, want: 30 * time.Second},
		{delay: -time.Minute, want: 30 * time.Second},
	} {
		test.Run(scenario.delay.String(), func(test *testing.T) {
			config, storage, repository := wave41NotificationConfig(test, notificationworker.Complete)
			ctx := systemTestContext()
			if err := enqueueRepositoryNotification(ctx, storage.FSStore, repository, "synthetic", "metadata only"); err != nil {
				test.Fatal(err)
			}
			config.Sender = &wave41NotificationSender{prepare: func(context.Context, outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
				return &wave41NotificationRequest{send: func(context.Context) (outbound.NotificationResponse, error) {
					return outbound.NotificationResponse{HTTPStatus: 503, RetryAfter: scenario.delay}, nil
				}}, nil
			}}
			worker, err := NewNotificationWorker(config)
			if err != nil || worker == nil {
				test.Fatal(err)
			}
			if worked, err := worker.Process(ctx); !worked || err != nil {
				test.Fatal(worked, err)
			}
			jobs, err := storage.FSStore.ListNotifications(ctx, repository.ID)
			if err != nil || len(jobs) != 1 || jobs[0].State != "retrying" || jobs[0].Reason != "http_retryable" || jobs[0].HTTPStatus != 503 || jobs[0].Attempts != 1 || !jobs[0].LeaseUntil.IsZero() {
				test.Fatal("sender response changed HTTP outcome", jobs, err)
			}
			if delay := jobs[0].NextAttempt.Sub(jobs[0].UpdatedAt); delay != scenario.want {
				test.Fatal("sender response lost Retry-After/backoff/cap", delay, scenario.want)
			}
		})
	}
}

func TestWave41NativeNotificationSenderFailuresPreserveOutcomes(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		reason string
		state  string
		onSend bool
	}{
		{name: "invalid-destination", reason: "invalid_destination", state: "attention"},
		{name: "encoding", reason: "encoding_failed", state: "retrying"},
		{name: "prepare-transport", reason: "transport_failed", state: "retrying"},
		{name: "send-transport", reason: "transport_failed", state: "retrying", onSend: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			config, storage, repository := wave41NotificationConfig(test, notificationworker.Complete)
			ctx := systemTestContext()
			if err := enqueueRepositoryNotification(ctx, storage.FSStore, repository, "synthetic", "metadata only"); err != nil {
				test.Fatal(err)
			}
			cause := errors.New("sender failure")
			failure := &outbound.NotificationSendError{Reason: scenario.reason, Cause: cause}
			if !errors.Is(failure, cause) {
				test.Fatal("sender failure discarded its cause")
			}
			config.Sender = &wave41NotificationSender{prepare: func(context.Context, outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
				storage.calls = append(storage.calls, "prepare")
				if !scenario.onSend {
					return nil, failure
				}
				return &wave41NotificationRequest{send: func(context.Context) (outbound.NotificationResponse, error) {
					storage.calls = append(storage.calls, "send")
					return outbound.NotificationResponse{}, failure
				}}, nil
			}}
			worker, err := NewNotificationWorker(config)
			if err != nil || worker == nil {
				test.Fatal(err)
			}
			if worked, err := worker.Process(ctx); !worked || err != nil {
				test.Fatal(worked, err)
			}
			wantCalls := []string{"claim", "repository", "prepare", "finish"}
			if scenario.onSend {
				wantCalls = []string{"claim", "repository", "prepare", "guard", "send", "finish"}
			}
			if !reflect.DeepEqual(storage.calls, wantCalls) {
				test.Fatal("sender failure changed guard/egress sequence", storage.calls)
			}
			jobs, err := storage.FSStore.ListNotifications(ctx, repository.ID)
			if err != nil || len(jobs) != 1 || jobs[0].State != scenario.state || jobs[0].Reason != scenario.reason || jobs[0].HTTPStatus != 0 || jobs[0].Attempts != 1 || !jobs[0].LeaseUntil.IsZero() {
				test.Fatal("sender failure changed persisted outcome", jobs, err)
			}
			if scenario.state == "retrying" && jobs[0].NextAttempt.Before(jobs[0].UpdatedAt.Add(30*time.Second)) {
				test.Fatal("sender failure lost original retry backoff", jobs)
			}
		})
	}
}

type wave41MaintenanceStore struct {
	outbound.RepositoryStore
	outbound.RuntimeStore
	calls []string
}

type wave41StorageReconciler interface {
	ReconcileNextStorageUsage(context.Context) (bool, error)
}

func (storage *wave41MaintenanceStore) PruneRuntimeState(context.Context, time.Time) error {
	storage.calls = append(storage.calls, "runtime")
	return nil
}

func (storage *wave41MaintenanceStore) ReconcileNextStorageUsage(context.Context) (bool, error) {
	storage.calls = append(storage.calls, "storage")
	return false, nil
}

func TestWave41MaintenanceFactoryProfilesAndRequiredRepositoryAdapter(test *testing.T) {
	storage := store.NewFSStore(test.TempDir())
	for _, profile := range []systemmaintenance.Profile{systemmaintenance.Compatibility, systemmaintenance.Complete} {
		test.Run(string(profile), func(test *testing.T) {
			capabilities := &wave41MaintenanceStore{RepositoryStore: storage, RuntimeStore: storage}
			config := MaintenanceWorkersConfig{Profile: profile, Repositories: capabilities}
			workers, err := NewMaintenanceWorkers(config)
			if err != nil || workers == nil || workers.Runtime == nil || workers.Storage == nil {
				test.Fatal("complete maintenance capabilities rejected", err)
			}
			if len(capabilities.calls) != 0 {
				test.Fatal("constructor executed maintenance", capabilities.calls)
			}
			for _, typed := range []bool{false, true} {
				invalid := config
				invalid.Repositories = nil
				if typed {
					invalid.Repositories = (*wave41MaintenanceStore)(nil)
					value := reflect.ValueOf(invalid.Repositories)
					if value.Kind() != reflect.Pointer || !value.IsNil() {
						test.Fatal("typed-nil repository fixture is not dynamically nil")
					}
				}
				if result, err := NewMaintenanceWorkers(invalid); result != nil || err == nil {
					test.Fatal("maintenance accepted missing repository adapter", typed, err)
				}
			}
			for _, unknown := range []systemmaintenance.Profile{"", "future-profile"} {
				invalid := config
				invalid.Profile = unknown
				if result, err := NewMaintenanceWorkers(invalid); result != nil || !errors.Is(err, domain.ErrValidation) {
					test.Fatal("unknown maintenance profile accepted", unknown, err)
				}
			}
			if len(capabilities.calls) != 0 {
				test.Fatal("rejected constructor executed maintenance", capabilities.calls)
			}
		})
	}
}

func TestWave41MaintenanceCompatibilityKeepsUnsupportedOwnersAndCompleteRejectsFS(test *testing.T) {
	storage := store.NewFSStore(test.TempDir())
	complete := &wave41MaintenanceStore{RepositoryStore: storage, RuntimeStore: storage}
	for _, capabilities := range []struct {
		name         string
		repositories outbound.RepositoryStore
	}{
		{name: "FS-runtime-without-storage", repositories: storage},
		{name: "no-runtime-or-storage", repositories: struct{ outbound.RepositoryStore }{RepositoryStore: storage}},
		{name: "storage-without-runtime", repositories: struct {
			outbound.RepositoryStore
			wave41StorageReconciler
		}{RepositoryStore: storage, wave41StorageReconciler: complete}},
	} {
		test.Run(capabilities.name, func(test *testing.T) {
			config := MaintenanceWorkersConfig{Profile: systemmaintenance.Compatibility, Repositories: capabilities.repositories}
			workers, err := NewMaintenanceWorkers(config)
			if err != nil || workers == nil || workers.Runtime == nil || workers.Storage == nil {
				test.Fatal("compatibility lost unsupported maintenance owner no-op", err)
			}
			if capabilities.name != "storage-without-runtime" {
				workers.Storage.Run(context.Background())
			}
			if capabilities.name != "FS-runtime-without-storage" {
				workers.Runtime.Run(context.Background())
			}
			config.Profile = systemmaintenance.Complete
			if workers, err := NewMaintenanceWorkers(config); workers != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
				test.Fatal("complete maintenance fabricated unsupported capabilities", err)
			}
		})
	}
	if len(complete.calls) != 0 {
		test.Fatal("constructor executed unsupported maintenance", complete.calls)
	}
}
