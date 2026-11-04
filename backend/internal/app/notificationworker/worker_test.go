package notificationworker_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/notificationworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type callerKey struct{}
type scopeKey struct{}

type workerScope struct {
	calls int
}

func (scope *workerScope) WorkerContext(ctx context.Context) context.Context {
	scope.calls++
	return context.WithValue(ctx, scopeKey{}, scope)
}

type notificationJobs struct {
	claim    func(context.Context, time.Time, time.Duration) (outbound.NotificationDelivery, error)
	finish   func(context.Context, domain.NotificationJob, time.Time) error
	claims   atomic.Int64
	finishes atomic.Int64
}

func (jobs *notificationJobs) ClaimNotification(ctx context.Context, now time.Time, lease time.Duration) (outbound.NotificationDelivery, error) {
	jobs.claims.Add(1)
	return jobs.claim(ctx, now, lease)
}

func (jobs *notificationJobs) FinishNotification(ctx context.Context, job domain.NotificationJob, now time.Time) error {
	jobs.finishes.Add(1)
	return jobs.finish(ctx, job, now)
}

func (*notificationJobs) EnqueueNotification(context.Context, outbound.NotificationDelivery) error {
	panic("worker must not enqueue notifications")
}

func (*notificationJobs) ListNotifications(context.Context, string) ([]domain.NotificationJob, error) {
	panic("worker must not list notifications")
}

func (*notificationJobs) RetryNotification(context.Context, string, string, string, time.Time) error {
	panic("worker must not request an operator retry")
}

type repositoryReader struct {
	get   func(context.Context, string) (domain.Repository, error)
	reads int
}

func (reader *repositoryReader) GetRepository(ctx context.Context, repositoryID string) (domain.Repository, error) {
	reader.reads++
	return reader.get(ctx, repositoryID)
}

type deliveryGuard struct {
	validate func(context.Context, outbound.NotificationDelivery, time.Time) error
	calls    int
}

func (guard *deliveryGuard) ValidateNotificationDelivery(ctx context.Context, delivery outbound.NotificationDelivery, now time.Time) error {
	guard.calls++
	return guard.validate(ctx, delivery, now)
}

type notificationSender struct {
	prepare func(context.Context, outbound.NotificationDelivery) (outbound.NotificationRequest, error)
	calls   int
}

func (sender *notificationSender) Prepare(ctx context.Context, delivery outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
	sender.calls++
	return sender.prepare(ctx, delivery)
}

type notificationRequest struct {
	send  func(context.Context) (outbound.NotificationResponse, error)
	calls int
}

func (request *notificationRequest) Send(ctx context.Context) (outbound.NotificationResponse, error) {
	request.calls++
	return request.send(ctx)
}

type notificationFixture struct {
	ctx           context.Context
	scope         *workerScope
	jobs          *notificationJobs
	repositories  *repositoryReader
	guard         *deliveryGuard
	sender        *notificationSender
	request       *notificationRequest
	delivery      outbound.NotificationDelivery
	repository    domain.Repository
	response      outbound.NotificationResponse
	prepared      outbound.NotificationRequest
	claimErr      error
	repositoryErr error
	prepareErr    error
	guardErr      error
	sendErr       error
	finishErr     error
	claimHook     func(context.Context, time.Time, time.Duration) (outbound.NotificationDelivery, error)
	prepareHook   func(context.Context, outbound.NotificationDelivery) (outbound.NotificationRequest, error)
	guardHook     func(context.Context, outbound.NotificationDelivery, time.Time) error
	sendHook      func(context.Context) (outbound.NotificationResponse, error)
	finishHook    func(context.Context, domain.NotificationJob, time.Time) error
	order         []string
	claimCtx      context.Context
	repositoryCtx context.Context
	prepareCtx    context.Context
	guardCtx      context.Context
	sendCtx       context.Context
	finishCtx     context.Context
	claimNow      time.Time
	guardNow      time.Time
	finishNow     time.Time
	lease         time.Duration
	finished      domain.NotificationJob
}

func newFixture(test *testing.T) *notificationFixture {
	test.Helper()
	now := time.Now().UTC()
	fixture := &notificationFixture{
		ctx:      inbound.WithRepositoryActor(context.WithValue(context.Background(), callerKey{}, "request-value"), "caller"),
		scope:    &workerScope{},
		response: outbound.NotificationResponse{HTTPStatus: 204},
		delivery: outbound.NotificationDelivery{
			Destination: "https://example.test/private-credential",
			Job: domain.NotificationJob{
				ID: "event-stable", RepositoryID: "repository", Kind: "ref_updated", Text: "metadata only\n\"branch\"",
				State: "sending", Attempts: 1, Version: 41, Reason: "previous", HTTPStatus: 503,
				CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute),
				NextAttempt: now.Add(-time.Minute), LeaseUntil: now.Add(2 * time.Minute),
			},
		},
	}
	fixture.repository = domain.Repository{ID: fixture.delivery.Job.RepositoryID, WebhookURL: fixture.delivery.Destination}
	record := func(stage string, ctx context.Context) {
		test.Helper()
		fixture.order = append(fixture.order, stage)
		if ctx.Value(scopeKey{}) != fixture.scope || ctx.Value(callerKey{}) != "request-value" {
			test.Error("worker lost the supplied scope or caller value", stage)
		}
		actor, system := inbound.RepositoryActor(ctx)
		expectedActor, expectedSystem := inbound.RepositoryActor(fixture.ctx)
		if actor != expectedActor || system != expectedSystem {
			test.Error("worker changed caller authority", stage, actor, system)
		}
	}
	fixture.jobs = &notificationJobs{
		claim: func(ctx context.Context, now time.Time, lease time.Duration) (outbound.NotificationDelivery, error) {
			record("claim", ctx)
			fixture.claimCtx, fixture.claimNow, fixture.lease = ctx, now, lease
			if fixture.claimHook != nil {
				return fixture.claimHook(ctx, now, lease)
			}
			return fixture.delivery, fixture.claimErr
		},
		finish: func(ctx context.Context, job domain.NotificationJob, now time.Time) error {
			record("finish", ctx)
			fixture.finishCtx, fixture.finished, fixture.finishNow = ctx, job, now
			if fixture.finishHook != nil {
				return fixture.finishHook(ctx, job, now)
			}
			return fixture.finishErr
		},
	}
	fixture.repositories = &repositoryReader{get: func(ctx context.Context, repositoryID string) (domain.Repository, error) {
		record("repository", ctx)
		fixture.repositoryCtx = ctx
		if repositoryID != fixture.delivery.Job.RepositoryID {
			test.Error("repository lookup escaped the claimed job", repositoryID)
		}
		return fixture.repository, fixture.repositoryErr
	}}
	fixture.guard = &deliveryGuard{validate: func(ctx context.Context, delivery outbound.NotificationDelivery, now time.Time) error {
		record("guard", ctx)
		fixture.guardCtx, fixture.guardNow = ctx, now
		if delivery != fixture.delivery {
			test.Error("guard did not receive the exact claimed delivery", delivery)
		}
		if fixture.guardHook != nil {
			return fixture.guardHook(ctx, delivery, now)
		}
		return fixture.guardErr
	}}
	fixture.request = &notificationRequest{send: func(ctx context.Context) (outbound.NotificationResponse, error) {
		record("send", ctx)
		fixture.sendCtx = ctx
		if fixture.sendHook != nil {
			return fixture.sendHook(ctx)
		}
		return fixture.response, fixture.sendErr
	}}
	fixture.prepared = fixture.request
	fixture.sender = &notificationSender{prepare: func(ctx context.Context, delivery outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
		record("prepare", ctx)
		fixture.prepareCtx = ctx
		if delivery != fixture.delivery {
			test.Error("sender did not receive the exact claimed delivery", delivery)
		}
		if fixture.prepareHook != nil {
			return fixture.prepareHook(ctx, delivery)
		}
		return fixture.prepared, fixture.prepareErr
	}}
	return fixture
}

func (fixture *notificationFixture) config(profile notificationworker.Profile) notificationworker.Config {
	return notificationworker.Config{
		Profile: profile, Scope: fixture.scope, Jobs: fixture.jobs,
		Repositories: fixture.repositories, Guard: fixture.guard, Sender: fixture.sender,
	}
}

func (fixture *notificationFixture) worker(test *testing.T) *notificationworker.Worker {
	test.Helper()
	worker, err := notificationworker.New(fixture.config(notificationworker.Complete))
	if err != nil {
		test.Fatal(err)
	}
	return worker
}

func (fixture *notificationFixture) assertOrder(test *testing.T, expected ...string) {
	test.Helper()
	if !reflect.DeepEqual(fixture.order, expected) {
		test.Fatal("unexpected operation order", fixture.order, expected)
	}
}

func (fixture *notificationFixture) assertFinished(test *testing.T, state, reason string, status int, delay time.Duration) {
	test.Helper()
	job := fixture.finished
	claimed := fixture.delivery.Job
	if fixture.jobs.finishes.Load() != 1 || job.State != state || job.Reason != reason || job.HTTPStatus != status {
		test.Fatal("unexpected durable outcome", fixture.jobs.finishes.Load(), job)
	}
	if job.ID != claimed.ID || job.RepositoryID != claimed.RepositoryID || job.Kind != claimed.Kind || job.Text != claimed.Text || job.Attempts != claimed.Attempts || job.Version != claimed.Version || job.CreatedAt != claimed.CreatedAt {
		test.Fatal("finish changed the claimed identity, payload, attempt or fence", job, claimed)
	}
	if fixture.lease != 2*time.Minute || fixture.claimNow.Location() != time.UTC || fixture.finishNow.Location() != time.UTC {
		test.Fatal("claim lease or UTC timestamps changed", fixture.lease, fixture.claimNow, fixture.finishNow)
	}
	if job.UpdatedAt != fixture.finishNow || job.NextAttempt.Sub(job.UpdatedAt) != delay || !job.LeaseUntil.IsZero() || fixture.finishNow.Before(fixture.claimNow) {
		test.Fatal("finish did not release the lease with the expected retry time", job, delay)
	}
	if fixture.finishCtx != fixture.claimCtx {
		test.Fatal("finish did not use the original outer worker context")
	}
}

func TestNewValidatesProfilesAndDeclaredDependencies(test *testing.T) {
	var nilScope *workerScope
	var nilJobs *notificationJobs
	var nilRepositories *repositoryReader
	var nilGuard *deliveryGuard
	var nilSender *notificationSender
	for _, profile := range []notificationworker.Profile{notificationworker.Compatibility, notificationworker.Complete} {
		for _, candidate := range []struct {
			name     string
			set      func(*notificationworker.Config)
			required bool
		}{
			{name: "scope", set: func(config *notificationworker.Config) { config.Scope = nil }, required: true},
			{name: "jobs", set: func(config *notificationworker.Config) { config.Jobs = nil }},
			{name: "repositories", set: func(config *notificationworker.Config) { config.Repositories = nil }},
			{name: "guard", set: func(config *notificationworker.Config) { config.Guard = nil }},
			{name: "sender", set: func(config *notificationworker.Config) { config.Sender = nil }},
			{name: "typed-nil-scope", set: func(config *notificationworker.Config) { config.Scope = nilScope }, required: true},
			{name: "typed-nil-jobs", set: func(config *notificationworker.Config) { config.Jobs = nilJobs }, required: true},
			{name: "typed-nil-repositories", set: func(config *notificationworker.Config) { config.Repositories = nilRepositories }, required: true},
			{name: "typed-nil-guard", set: func(config *notificationworker.Config) { config.Guard = nilGuard }, required: true},
			{name: "typed-nil-sender", set: func(config *notificationworker.Config) { config.Sender = nilSender }, required: true},
		} {
			test.Run(string(profile)+"/"+candidate.name, func(test *testing.T) {
				fixture := newFixture(test)
				config := fixture.config(profile)
				candidate.set(&config)
				worker, err := notificationworker.New(config)
				if profile == notificationworker.Complete || candidate.required {
					if worker != nil || !errors.Is(err, domain.ErrValidation) {
						test.Fatal("invalid construction was accepted", worker, err)
					}
				} else if worker == nil || err != nil {
					test.Fatal("explicit compatibility omission was rejected", worker, err)
				}
				if fixture.scope.calls != 0 || len(fixture.order) != 0 {
					test.Fatal("construction invoked a dependency", fixture.scope.calls, fixture.order)
				}
			})
		}
	}
	for _, profile := range []notificationworker.Profile{"", "unknown"} {
		fixture := newFixture(test)
		if worker, err := notificationworker.New(fixture.config(profile)); worker != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("undeclared profile was accepted", worker, err)
		}
	}
}

func TestNewDoesNotStartOrReplaceDependencies(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newFixture(test)
		worker := fixture.worker(test)
		synctest.Wait()
		if fixture.scope.calls != 0 || len(fixture.order) != 0 {
			test.Fatal("constructor started work", fixture.scope.calls, fixture.order)
		}
		fixture.response.HTTPStatus = 202
		worked, err := worker.Process(fixture.ctx)
		if !worked || err != nil || fixture.scope.calls != 1 || fixture.jobs.claims.Load() != 1 || fixture.repositories.reads != 1 || fixture.sender.calls != 1 || fixture.guard.calls != 1 || fixture.request.calls != 1 {
			test.Fatal("worker did not reuse the exact supplied dependencies", worked, err, fixture.order)
		}
		fixture.assertFinished(test, "delivered", "", 202, 0)
	})
}

func TestCompatibilityNoopAndMissingSenderPrecedeClaim(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		missing string
		failure error
	}{
		{name: "missing-jobs", missing: "jobs"},
		{name: "missing-repositories", missing: "repositories"},
		{name: "both-missing", missing: "both"},
		{name: "missing-sender", missing: "sender", failure: domain.ErrCatalogUnsupported},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			fixture := newFixture(test)
			config := fixture.config(notificationworker.Compatibility)
			config.Sender = nil
			switch candidate.missing {
			case "jobs":
				config.Jobs = nil
			case "repositories":
				config.Repositories = nil
			case "both":
				config.Jobs, config.Repositories = nil, nil
			}
			worker, err := notificationworker.New(config)
			if err != nil {
				test.Fatal(err)
			}
			worked, err := worker.Process(fixture.ctx)
			if worked || err != candidate.failure || fixture.jobs.claims.Load() != 0 || len(fixture.order) != 0 || fixture.scope.calls != 1 {
				test.Fatal("compatibility setup leased work or changed no-op/error precedence", worked, err, fixture.order)
			}
		})
	}
}

func TestProcessClaimNoWorkAndErrors(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		claim   error
		failure error
	}{
		{name: "idle", claim: domain.ErrNotFound},
		{name: "wrapped-idle", claim: fmt.Errorf("no eligible claim: %w", domain.ErrNotFound)},
		{name: "forbidden", claim: domain.ErrForbidden, failure: domain.ErrForbidden},
		{name: "unsupported", claim: domain.ErrCatalogUnsupported, failure: domain.ErrCatalogUnsupported},
		{name: "canceled", claim: context.Canceled, failure: context.Canceled},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			fixture := newFixture(test)
			fixture.claimErr = candidate.claim
			worked, err := fixture.worker(test).Process(fixture.ctx)
			if worked || err != candidate.failure || fixture.lease != 2*time.Minute || fixture.claimNow.Location() != time.UTC {
				test.Fatal("claim outcome, lease or UTC contract changed", worked, err, fixture.lease, fixture.claimNow)
			}
			fixture.assertOrder(test, "claim")
		})
	}
}

func TestProcessChecksCurrentRepositoryAndSchemeBeforePrepare(test *testing.T) {
	for _, candidate := range []struct {
		name      string
		configure func(*notificationFixture)
		state     string
		reason    string
		delay     time.Duration
	}{
		{name: "removed", configure: func(fixture *notificationFixture) {
			fixture.repositoryErr = fmt.Errorf("deleted: %w", domain.ErrNotFound)
		}, state: "attention", reason: "destination_disabled"},
		{name: "unavailable", configure: func(fixture *notificationFixture) { fixture.repositoryErr = domain.ErrIntegrity }, state: "retrying", reason: "configuration_unavailable", delay: 30 * time.Second},
		{name: "archived", configure: func(fixture *notificationFixture) { fixture.repository.Archived = true }, state: "attention", reason: "destination_disabled"},
		{name: "disabled", configure: func(fixture *notificationFixture) { fixture.repository.WebhookURL = "" }, state: "attention", reason: "destination_disabled"},
		{name: "changed", configure: func(fixture *notificationFixture) { fixture.repository.WebhookURL += "/replacement" }, state: "attention", reason: "destination_changed"},
		{name: "scheme", configure: func(fixture *notificationFixture) {
			fixture.delivery.Destination = "ftp://example.test/hook"
			fixture.repository.WebhookURL = fixture.delivery.Destination
		}, state: "attention", reason: "invalid_destination"},
		{name: "missing-host", configure: func(fixture *notificationFixture) {
			fixture.delivery.Destination = "https:///hook"
			fixture.repository.WebhookURL = fixture.delivery.Destination
		}, state: "attention", reason: "invalid_destination"},
		{name: "malformed", configure: func(fixture *notificationFixture) {
			fixture.delivery.Destination = "https://example.test/%zz"
			fixture.repository.WebhookURL = fixture.delivery.Destination
		}, state: "attention", reason: "invalid_destination"},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			fixture := newFixture(test)
			candidate.configure(fixture)
			if worked, err := fixture.worker(test).Process(fixture.ctx); !worked || err != nil {
				test.Fatal(worked, err)
			}
			fixture.assertOrder(test, "claim", "repository", "finish")
			fixture.assertFinished(test, candidate.state, candidate.reason, 0, candidate.delay)
		})
	}
	for _, destination := range []string{"http://example.test/hook", "https://example.test/hook", "http://127.0.0.1/hook"} {
		test.Run(destination, func(test *testing.T) {
			fixture := newFixture(test)
			fixture.delivery.Destination, fixture.repository.WebhookURL = destination, destination
			if worked, err := fixture.worker(test).Process(fixture.ctx); !worked || err != nil || fixture.request.calls != 1 {
				test.Fatal("pure scheme precheck replaced adapter address validation", worked, err)
			}
		})
	}
}

func TestProcessRereadsDestinationOnEachClaim(test *testing.T) {
	fixture := newFixture(test)
	worker := fixture.worker(test)
	if worked, err := worker.Process(fixture.ctx); !worked || err != nil {
		test.Fatal(worked, err)
	}
	fixture.repository.WebhookURL += "/replacement"
	if worked, err := worker.Process(fixture.ctx); !worked || err != nil {
		test.Fatal(worked, err)
	}
	if fixture.repositories.reads != 2 || fixture.sender.calls != 1 || fixture.request.calls != 1 || fixture.finished.Reason != "destination_changed" || fixture.finished.State != "attention" {
		test.Fatal("worker reused stale destination configuration", fixture.order, fixture.finished)
	}
}

func TestProcessPreservesCallerAndPreparedPayloadOrder(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newFixture(test)
		started := time.Now().UTC()
		fixture.prepareHook = func(ctx context.Context, delivery outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
			deadline, present := ctx.Deadline()
			if !present || !deadline.Equal(started.Add(5*time.Second)) || delivery != fixture.delivery {
				test.Error("prepare did not receive the original payload and five-second context", deadline, delivery)
			}
			time.Sleep(2 * time.Second)
			return fixture.request, nil
		}
		fixture.guardHook = func(ctx context.Context, delivery outbound.NotificationDelivery, now time.Time) error {
			if ctx != fixture.prepareCtx || delivery != fixture.delivery || now != started.Add(2*time.Second) || now.Location() != time.UTC || fixture.request.calls != 0 || fixture.jobs.finishes.Load() != 0 {
				test.Error("guard was not the current final boundary before send", now, fixture.order)
			}
			return nil
		}
		fixture.sendHook = func(ctx context.Context) (outbound.NotificationResponse, error) {
			if ctx != fixture.prepareCtx || ctx != fixture.guardCtx || fixture.guard.calls != 1 || fixture.jobs.finishes.Load() != 0 {
				test.Error("send was not on the exact prepared and guarded context", fixture.order)
			}
			time.Sleep(time.Second)
			return fixture.response, nil
		}
		if worked, err := fixture.worker(test).Process(fixture.ctx); !worked || err != nil {
			test.Fatal(worked, err)
		}
		fixture.assertOrder(test, "claim", "repository", "prepare", "guard", "send", "finish")
		fixture.assertFinished(test, "delivered", "", 204, 0)
		if fixture.repositoryCtx != fixture.claimCtx || fixture.finishCtx == fixture.sendCtx || fixture.prepareCtx.Err() != context.Canceled || fixture.ctx.Err() != nil {
			test.Fatal("send scope escaped or canceled the outer context")
		}
	})
	for _, actor := range []string{"", "caller", "system"} {
		test.Run("actor/"+actor, func(test *testing.T) {
			fixture := newFixture(test)
			fixture.ctx = context.WithValue(context.Background(), callerKey{}, "request-value")
			switch actor {
			case "caller":
				fixture.ctx = inbound.WithRepositoryActor(fixture.ctx, actor)
			case "system":
				fixture.ctx = inbound.WithSystemActor(fixture.ctx)
			}
			if worked, err := fixture.worker(test).Process(fixture.ctx); !worked || err != nil {
				test.Fatal(worked, err)
			}
		})
	}
}

func TestProcessPreparationFailuresAndMissingRequest(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		failure error
		state   string
		reason  string
		delay   time.Duration
	}{
		{name: "invalid-destination", failure: fmt.Errorf("prepare: %w", &outbound.NotificationSendError{Reason: "invalid_destination", Cause: domain.ErrValidation}), state: "attention", reason: "invalid_destination"},
		{name: "encoding", failure: fmt.Errorf("prepare: %w", &outbound.NotificationSendError{Reason: "encoding_failed", Cause: domain.ErrIntegrity}), state: "retrying", reason: "encoding_failed", delay: 30 * time.Second},
		{name: "transport", failure: &outbound.NotificationSendError{Reason: "transport_failed", Cause: context.DeadlineExceeded}, state: "retrying", reason: "transport_failed", delay: 30 * time.Second},
		{name: "unclassified", failure: domain.ErrIntegrity, state: "retrying", reason: "transport_failed", delay: 30 * time.Second},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			fixture := newFixture(test)
			fixture.prepareErr = candidate.failure
			if worked, err := fixture.worker(test).Process(fixture.ctx); !worked || err != nil {
				test.Fatal(worked, err)
			}
			fixture.assertOrder(test, "claim", "repository", "prepare", "finish")
			fixture.assertFinished(test, candidate.state, candidate.reason, 0, candidate.delay)
		})
	}
	var nilRequest *notificationRequest
	for _, candidate := range []struct {
		name    string
		request outbound.NotificationRequest
	}{
		{name: "nil"},
		{name: "typed-nil", request: nilRequest},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			fixture := newFixture(test)
			fixture.prepared = candidate.request
			worked, err := fixture.worker(test).Process(fixture.ctx)
			if !worked || !errors.Is(err, domain.ErrValidation) || fixture.jobs.finishes.Load() != 0 || fixture.guard.calls != 0 || fixture.request.calls != 0 {
				test.Fatal("absent prepared request crossed a durable or egress boundary", worked, err, fixture.order)
			}
			fixture.assertOrder(test, "claim", "repository", "prepare")
		})
	}
}

func TestProcessGuardFailuresNeverFinishOrSend(test *testing.T) {
	for _, failure := range []error{
		domain.ErrForbidden, domain.ErrRefConflict, domain.ErrNotFound,
		domain.ErrDocumentIdentityUpgradeRequired, context.Canceled, context.DeadlineExceeded,
		fmt.Errorf("guard: %w", domain.ErrIntegrity),
	} {
		test.Run(failure.Error(), func(test *testing.T) {
			fixture := newFixture(test)
			fixture.guardErr = failure
			worked, err := fixture.worker(test).Process(fixture.ctx)
			if !worked || err != failure || fixture.jobs.finishes.Load() != 0 || fixture.request.calls != 0 {
				test.Fatal("guard error was transformed, finished or sent", worked, err, fixture.order)
			}
			fixture.assertOrder(test, "claim", "repository", "prepare", "guard")
		})
	}
	test.Run("missing-guard", func(test *testing.T) {
		fixture := newFixture(test)
		config := fixture.config(notificationworker.Compatibility)
		config.Guard = nil
		worker, err := notificationworker.New(config)
		if err != nil {
			test.Fatal(err)
		}
		worked, err := worker.Process(fixture.ctx)
		if !worked || err != domain.ErrForbidden || fixture.jobs.finishes.Load() != 0 || fixture.request.calls != 0 {
			test.Fatal("missing compatibility guard did not fail closed", worked, err)
		}
		fixture.assertOrder(test, "claim", "repository", "prepare")
	})
	test.Run("revoked-after-prepare", func(test *testing.T) {
		fixture := newFixture(test)
		fixture.prepareHook = func(context.Context, outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
			fixture.repository.WebhookURL = ""
			return fixture.request, nil
		}
		fixture.guardHook = func(context.Context, outbound.NotificationDelivery, time.Time) error {
			if fixture.repository.WebhookURL == "" {
				return domain.ErrForbidden
			}
			return nil
		}
		worked, err := fixture.worker(test).Process(fixture.ctx)
		if !worked || err != domain.ErrForbidden || fixture.jobs.finishes.Load() != 0 || fixture.request.calls != 0 {
			test.Fatal("preparation bypassed the current egress guard", worked, err, fixture.order)
		}
		fixture.assertOrder(test, "claim", "repository", "prepare", "guard")
	})
}

func TestProcessGuardObservesLeaseExpirationAfterPrepare(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newFixture(test)
		fixture.delivery.Job.LeaseUntil = time.Now().UTC().Add(2 * time.Second)
		fixture.prepareHook = func(context.Context, outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
			time.Sleep(3 * time.Second)
			return fixture.request, nil
		}
		fixture.guardHook = func(ctx context.Context, delivery outbound.NotificationDelivery, now time.Time) error {
			if ctx.Err() != nil || delivery != fixture.delivery || !now.After(delivery.Job.LeaseUntil) {
				test.Error("guard did not see the current expired lease on the prepared delivery", delivery, now)
				return nil
			}
			return domain.ErrRefConflict
		}
		worked, err := fixture.worker(test).Process(fixture.ctx)
		if !worked || err != domain.ErrRefConflict || fixture.jobs.finishes.Load() != 0 || fixture.request.calls != 0 || fixture.lease != 2*time.Minute {
			test.Fatal("expired claim was finished or sent", worked, err, fixture.order)
		}
		fixture.assertOrder(test, "claim", "repository", "prepare", "guard")
	})
}

func TestProcessHTTPRetryAfterAndAttemptOutcomes(test *testing.T) {
	for _, candidate := range []struct {
		name       string
		status     int
		attempts   int
		retryAfter time.Duration
		state      string
		reason     string
		delay      time.Duration
	}{
		{name: "success-lower", status: 200, attempts: 1, state: "delivered"},
		{name: "success-upper", status: 299, attempts: 1, state: "delivered"},
		{name: "informational", status: 199, attempts: 1, state: "attention", reason: "http_rejected"},
		{name: "redirect", status: 302, attempts: 1, state: "attention", reason: "http_rejected"},
		{name: "rejected", status: 400, attempts: 1, retryAfter: time.Hour, state: "attention", reason: "http_rejected"},
		{name: "request-timeout", status: 408, attempts: 1, state: "retrying", reason: "http_retryable", delay: 30 * time.Second},
		{name: "too-early", status: 425, attempts: 1, state: "retrying", reason: "http_retryable", delay: 30 * time.Second},
		{name: "rate-limited", status: 429, attempts: 1, retryAfter: 2 * time.Minute, state: "retrying", reason: "http_retryable", delay: 2 * time.Minute},
		{name: "server-error", status: 500, attempts: 1, state: "retrying", reason: "http_retryable", delay: 30 * time.Second},
		{name: "server-error-upper", status: 599, attempts: 1, state: "retrying", reason: "http_retryable", delay: 30 * time.Second},
		{name: "status-above-range", status: 600, attempts: 1, state: "retrying", reason: "http_retryable", delay: 30 * time.Second},
		{name: "past-retry-time", status: 503, attempts: 1, retryAfter: -time.Hour, state: "retrying", reason: "http_retryable", delay: 30 * time.Second},
		{name: "short-retry-after", status: 503, attempts: 1, retryAfter: time.Second, state: "retrying", reason: "http_retryable", delay: 30 * time.Second},
		{name: "retry-after-limit", status: 503, attempts: 1, retryAfter: 25 * time.Hour, state: "retrying", reason: "http_retryable", delay: 24 * time.Hour},
		{name: "fractional-retry-after", status: 503, attempts: 1, retryAfter: time.Minute + time.Millisecond, state: "retrying", reason: "http_retryable", delay: time.Minute + time.Millisecond},
		{name: "zero-attempt", status: 503, attempts: 0, state: "retrying", reason: "http_retryable", delay: 15 * time.Second},
		{name: "second-attempt", status: 503, attempts: 2, state: "retrying", reason: "http_retryable", delay: time.Minute},
		{name: "seventh-attempt", status: 503, attempts: 7, state: "retrying", reason: "http_retryable", delay: 32 * time.Minute},
		{name: "exhausted", status: 503, attempts: 8, retryAfter: time.Hour, state: "attention", reason: "attempts_exhausted"},
		{name: "last-success", status: 204, attempts: 8, state: "delivered"},
		{name: "last-rejection", status: 400, attempts: 8, state: "attention", reason: "http_rejected"},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			fixture := newFixture(test)
			fixture.delivery.Job.Attempts = candidate.attempts
			fixture.response = outbound.NotificationResponse{HTTPStatus: candidate.status, RetryAfter: candidate.retryAfter}
			if worked, err := fixture.worker(test).Process(fixture.ctx); !worked || err != nil {
				test.Fatal(worked, err)
			}
			fixture.assertFinished(test, candidate.state, candidate.reason, candidate.status, candidate.delay)
			fixture.assertOrder(test, "claim", "repository", "prepare", "guard", "send", "finish")
		})
	}
}

func TestProcessExhaustionAndTransportFailures(test *testing.T) {
	test.Run("over-attempt-limit", func(test *testing.T) {
		fixture := newFixture(test)
		fixture.delivery.Job.Attempts = 9
		if worked, err := fixture.worker(test).Process(fixture.ctx); !worked || err != nil {
			test.Fatal(worked, err)
		}
		fixture.assertOrder(test, "claim", "finish")
		fixture.assertFinished(test, "attention", "attempts_exhausted", 0, 0)
	})
	for _, stage := range []string{"repository", "prepare", "send"} {
		test.Run("last-attempt/"+stage, func(test *testing.T) {
			fixture := newFixture(test)
			fixture.delivery.Job.Attempts = 8
			switch stage {
			case "repository":
				fixture.repositoryErr = domain.ErrIntegrity
			case "prepare":
				fixture.prepareErr = &outbound.NotificationSendError{Reason: "encoding_failed", Cause: domain.ErrIntegrity}
			case "send":
				fixture.sendErr = domain.ErrIntegrity
			}
			if worked, err := fixture.worker(test).Process(fixture.ctx); !worked || err != nil {
				test.Fatal(worked, err)
			}
			fixture.assertFinished(test, "attention", "attempts_exhausted", 0, 0)
		})
	}
	for _, failure := range []error{domain.ErrIntegrity, context.Canceled, &outbound.NotificationSendError{Reason: "invalid_destination", Cause: domain.ErrValidation}} {
		test.Run("send/"+failure.Error(), func(test *testing.T) {
			fixture := newFixture(test)
			fixture.sendErr = failure
			fixture.response = outbound.NotificationResponse{HTTPStatus: 503, RetryAfter: time.Hour}
			if worked, err := fixture.worker(test).Process(fixture.ctx); !worked || err != nil {
				test.Fatal(worked, err)
			}
			fixture.assertFinished(test, "retrying", "transport_failed", 0, 30*time.Second)
		})
	}
	for _, failure := range []error{domain.ErrRefConflict, domain.ErrForbidden, domain.ErrDocumentIdentityUpgradeRequired, context.Canceled} {
		test.Run("finish/"+failure.Error(), func(test *testing.T) {
			fixture := newFixture(test)
			fixture.finishErr = failure
			worked, err := fixture.worker(test).Process(fixture.ctx)
			if !worked || err != failure || fixture.request.calls != 1 || fixture.jobs.finishes.Load() != 1 {
				test.Fatal("post-egress finish failure was retried, swallowed or transformed", worked, err, fixture.order)
			}
		})
	}
}

func TestProcessFinishFailurePreservesWorkedAndDoesNotRepeatEgress(test *testing.T) {
	for _, outcome := range []string{"delivered", "retrying", "attention", "exhausted"} {
		test.Run(outcome, func(test *testing.T) {
			fixture := newFixture(test)
			failure := fmt.Errorf("finish lost claim: %w", domain.ErrRefConflict)
			fixture.finishErr = failure
			switch outcome {
			case "retrying":
				fixture.prepareErr = &outbound.NotificationSendError{Reason: "encoding_failed", Cause: domain.ErrIntegrity}
			case "attention":
				fixture.repository.WebhookURL = ""
			case "exhausted":
				fixture.delivery.Job.Attempts = 9
			}
			worked, err := fixture.worker(test).Process(fixture.ctx)
			if !worked || err != failure || fixture.jobs.finishes.Load() != 1 || fixture.jobs.claims.Load() != 1 {
				test.Fatal("finish failure changed worked/error identity or repeated the durable operation", worked, err, fixture.order)
			}
			expectedSends := 0
			if outcome == "delivered" {
				expectedSends = 1
			}
			if fixture.request.calls != expectedSends {
				test.Fatal("finish failure crossed an additional egress boundary", fixture.request.calls, fixture.order)
			}
		})
	}
}

func TestProcessFiveSecondContextIncludesPreparationAndGuard(test *testing.T) {
	for _, stage := range []string{"prepare", "guard", "send"} {
		test.Run(stage, func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				fixture := newFixture(test)
				started := time.Now()
				switch stage {
				case "prepare":
					fixture.prepareHook = func(ctx context.Context, _ outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
						<-ctx.Done()
						return nil, ctx.Err()
					}
				case "guard":
					fixture.guardHook = func(ctx context.Context, _ outbound.NotificationDelivery, _ time.Time) error {
						<-ctx.Done()
						return ctx.Err()
					}
				case "send":
					fixture.sendHook = func(ctx context.Context) (outbound.NotificationResponse, error) {
						<-ctx.Done()
						return outbound.NotificationResponse{}, ctx.Err()
					}
				}
				worked, err := fixture.worker(test).Process(fixture.ctx)
				if !worked || time.Since(started) != 5*time.Second || fixture.ctx.Err() != nil || fixture.prepareCtx.Err() != context.DeadlineExceeded {
					test.Fatal("send deadline did not bound the requested stage or canceled its caller", worked, err, time.Since(started))
				}
				if stage == "guard" {
					if err != context.DeadlineExceeded || fixture.jobs.finishes.Load() != 0 || fixture.request.calls != 0 {
						test.Fatal("guard deadline was finished or sent", err, fixture.order)
					}
				} else {
					if err != nil {
						test.Fatal(err)
					}
					fixture.assertFinished(test, "retrying", "transport_failed", 0, 30*time.Second)
					if fixture.finishCtx.Err() != nil || fixture.finishCtx == fixture.prepareCtx {
						test.Fatal("finish inherited the expired send timeout")
					}
				}
			})
		})
	}
	synctest.Test(test, func(test *testing.T) {
		fixture := newFixture(test)
		started := time.Now()
		fixture.prepareHook = func(context.Context, outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
			time.Sleep(4 * time.Second)
			return fixture.request, nil
		}
		fixture.sendHook = func(ctx context.Context) (outbound.NotificationResponse, error) {
			deadline, present := ctx.Deadline()
			if !present || !deadline.Equal(started.Add(5*time.Second)) || ctx != fixture.prepareCtx || ctx != fixture.guardCtx {
				test.Error("send received a fresh timeout after preparation", deadline)
			}
			<-ctx.Done()
			return outbound.NotificationResponse{}, ctx.Err()
		}
		if worked, err := fixture.worker(test).Process(fixture.ctx); !worked || err != nil || time.Since(started) != 5*time.Second {
			test.Fatal("preparation time was excluded from the send limit", worked, err, time.Since(started))
		}
		fixture.assertFinished(test, "retrying", "transport_failed", 0, 30*time.Second)
	})
}

func TestProcessEarlierCallerDeadlineAndFinishCancellation(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newFixture(test)
		ctx, cancel := context.WithTimeout(fixture.ctx, time.Second)
		defer cancel()
		fixture.ctx = ctx
		started := time.Now()
		fixture.sendHook = func(ctx context.Context) (outbound.NotificationResponse, error) {
			deadline, present := ctx.Deadline()
			if !present || !deadline.Equal(started.Add(time.Second)) {
				test.Error("worker extended the earlier caller deadline", deadline)
			}
			<-ctx.Done()
			return outbound.NotificationResponse{}, ctx.Err()
		}
		fixture.finishHook = func(ctx context.Context, _ domain.NotificationJob, _ time.Time) error {
			return ctx.Err()
		}
		worked, err := fixture.worker(test).Process(fixture.ctx)
		if !worked || err != context.DeadlineExceeded || time.Since(started) != time.Second || fixture.finishCtx.Err() != context.DeadlineExceeded {
			test.Fatal("finish detached from the original canceled caller", worked, err, time.Since(started))
		}
		fixture.assertFinished(test, "retrying", "transport_failed", 0, 30*time.Second)
	})
}

func TestRunImmediateBoundedBatchAndThreeSecondPolling(test *testing.T) {
	for _, candidate := range []struct {
		name     string
		claims   int64
		finishes int64
	}{
		{name: "productive", claims: 20, finishes: 20},
		{name: "idle", claims: 1},
		{name: "claim-error", claims: 1},
		{name: "partial-idle", claims: 3, finishes: 2},
		{name: "finish-error", claims: 1, finishes: 1},
		{name: "guard-error", claims: 1},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				fixture := newFixture(test)
				switch candidate.name {
				case "idle":
					fixture.claimErr = domain.ErrNotFound
				case "claim-error":
					fixture.claimErr = domain.ErrIntegrity
				case "partial-idle":
					fixture.claimHook = func(context.Context, time.Time, time.Duration) (outbound.NotificationDelivery, error) {
						if fixture.jobs.claims.Load()%3 == 0 {
							return outbound.NotificationDelivery{}, domain.ErrNotFound
						}
						return fixture.delivery, nil
					}
				case "finish-error":
					fixture.finishErr = domain.ErrRefConflict
				case "guard-error":
					fixture.guardErr = domain.ErrForbidden
				}
				ctx, cancel := context.WithCancel(fixture.ctx)
				defer cancel()
				fixture.ctx = ctx
				worker := fixture.worker(test)
				done := make(chan struct{})
				go func() {
					defer close(done)
					worker.Run(ctx)
				}()
				synctest.Wait()
				claims, finishes := fixture.jobs.claims.Load(), fixture.jobs.finishes.Load()
				if claims != candidate.claims || finishes != candidate.finishes {
					test.Fatal("initial poll changed its immediate batch/error boundary", claims, finishes)
				}
				time.Sleep(3*time.Second - time.Nanosecond)
				synctest.Wait()
				claims, finishes = fixture.jobs.claims.Load(), fixture.jobs.finishes.Load()
				if claims != candidate.claims || finishes != candidate.finishes {
					test.Fatal("worker repolled before three seconds", claims, finishes)
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				claims, finishes = fixture.jobs.claims.Load(), fixture.jobs.finishes.Load()
				if claims != 2*candidate.claims || finishes != 2*candidate.finishes {
					test.Fatal("worker did not resume at exactly three seconds", claims, finishes)
				}
				cancel()
				synctest.Wait()
				select {
				case <-done:
				default:
					test.Fatal("worker did not exit its canceled poll")
				}
				if fixture.jobs.claims.Load() != claims || fixture.jobs.finishes.Load() != finishes {
					test.Fatal("worker processed more work after poll cancellation", fixture.order)
				}
			})
		})
	}
}

func TestRunCancellationReachesInFlightSendAndOuterFinish(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newFixture(test)
		ctx, cancel := context.WithCancel(fixture.ctx)
		defer cancel()
		fixture.ctx = ctx
		sendStarted := make(chan struct{})
		fixture.sendHook = func(ctx context.Context) (outbound.NotificationResponse, error) {
			close(sendStarted)
			<-ctx.Done()
			return outbound.NotificationResponse{}, ctx.Err()
		}
		fixture.finishHook = func(ctx context.Context, _ domain.NotificationJob, _ time.Time) error {
			return ctx.Err()
		}
		worker := fixture.worker(test)
		done := make(chan struct{})
		go func() {
			defer close(done)
			worker.Run(ctx)
		}()
		synctest.Wait()
		select {
		case <-sendStarted:
		default:
			test.Fatal("worker did not reach the first send immediately")
		}
		if fixture.jobs.claims.Load() != 1 || fixture.request.calls != 1 || fixture.jobs.finishes.Load() != 0 {
			test.Fatal("worker did not stop inside the first send", fixture.order)
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			test.Fatal("worker did not join after send cancellation")
		}
		if fixture.jobs.claims.Load() != 1 || fixture.prepareCtx != fixture.guardCtx || fixture.guardCtx != fixture.sendCtx || fixture.sendCtx.Err() != context.Canceled || fixture.finishCtx.Err() != context.Canceled {
			test.Fatal("cancellation escaped the original send/finish contexts", fixture.order)
		}
		fixture.assertFinished(test, "retrying", "transport_failed", 0, 30*time.Second)
	})
}
