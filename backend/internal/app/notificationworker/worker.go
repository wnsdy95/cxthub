package notificationworker

import (
	"context"
	"errors"
	"fmt"
	neturl "net/url"
	"reflect"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const notificationMaxAttempts = 8
const notificationLease = 2 * time.Minute

type Worker struct {
	scope        Scope
	jobs         outbound.NotificationStore
	repositories RepositoryReader
	guard        outbound.NotificationDeliveryGuard
	sender       outbound.NotificationSender
}

func New(config Config) (*Worker, error) {
	if config.Profile != Compatibility && config.Profile != Complete {
		return nil, fmt.Errorf("%w: notification worker profile is required", domain.ErrValidation)
	}
	if absent(config.Scope) {
		return nil, fmt.Errorf("%w: notification worker scope is required", domain.ErrValidation)
	}
	for _, dependency := range []struct {
		name  string
		value any
	}{
		{name: "jobs", value: config.Jobs},
		{name: "repositories", value: config.Repositories},
		{name: "guard", value: config.Guard},
		{name: "sender", value: config.Sender},
	} {
		if (config.Profile == Complete || dependency.value != nil) && absent(dependency.value) {
			return nil, fmt.Errorf("%w: notification worker %s is required", domain.ErrValidation, dependency.name)
		}
	}
	return &Worker{
		scope:        config.Scope,
		jobs:         config.Jobs,
		repositories: config.Repositories,
		guard:        config.Guard,
		sender:       config.Sender,
	}, nil
}

func absent(dependency any) bool {
	if dependency == nil {
		return true
	}
	value := reflect.ValueOf(dependency)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (worker *Worker) Process(ctx context.Context) (bool, error) {
	ctx = worker.scope.WorkerContext(ctx)
	if worker.jobs == nil || worker.repositories == nil {
		return false, nil
	}
	if worker.sender == nil {
		return false, domain.ErrCatalogUnsupported
	}
	delivery, err := worker.jobs.ClaimNotification(ctx, time.Now().UTC(), notificationLease)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	job := delivery.Job
	finish := func(state, reason string, status int, delay time.Duration) (bool, error) {
		now := time.Now().UTC()
		job.State = state
		job.Reason = reason
		job.HTTPStatus = status
		job.UpdatedAt = now
		job.LeaseUntil = time.Time{}
		job.NextAttempt = now.Add(delay)
		return true, worker.jobs.FinishNotification(ctx, job, now)
	}
	retry := func(reason string, status int, delay time.Duration) (bool, error) {
		if job.Attempts >= notificationMaxAttempts {
			return finish("attention", "attempts_exhausted", status, 0)
		}
		backoff := time.Duration(1<<min(job.Attempts, 10)) * 15 * time.Second
		if delay < backoff {
			delay = backoff
		}
		if delay > 24*time.Hour {
			delay = 24 * time.Hour
		}
		return finish("retrying", reason, status, delay)
	}
	if job.Attempts > notificationMaxAttempts {
		return finish("attention", "attempts_exhausted", 0, 0)
	}
	repository, err := worker.repositories.GetRepository(ctx, job.RepositoryID)
	if errors.Is(err, domain.ErrNotFound) {
		return finish("attention", "destination_disabled", 0, 0)
	}
	if err != nil {
		return retry("configuration_unavailable", 0, 0)
	}
	if repository.Archived || repository.WebhookURL == "" {
		return finish("attention", "destination_disabled", 0, 0)
	}
	if repository.WebhookURL != delivery.Destination {
		return finish("attention", "destination_changed", 0, 0)
	}
	if !webhookSchemeOK(delivery.Destination) {
		return finish("attention", "invalid_destination", 0, 0)
	}
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := worker.sender.Prepare(sendCtx, delivery)
	if err != nil {
		var failure *outbound.NotificationSendError
		if errors.As(err, &failure) && failure != nil {
			switch failure.Reason {
			case "invalid_destination":
				return finish("attention", "invalid_destination", 0, 0)
			case "encoding_failed":
				return retry("encoding_failed", 0, 0)
			}
		}
		return retry("transport_failed", 0, 0)
	}
	if absent(request) {
		return true, fmt.Errorf("%w: prepared notification request is required", domain.ErrValidation)
	}
	if worker.guard == nil {
		return true, domain.ErrForbidden
	}
	if err := worker.guard.ValidateNotificationDelivery(sendCtx, delivery, time.Now().UTC()); err != nil {
		return true, err
	}
	response, err := request.Send(sendCtx)
	if err != nil {
		return retry("transport_failed", 0, 0)
	}
	if response.HTTPStatus >= 200 && response.HTTPStatus < 300 {
		return finish("delivered", "", response.HTTPStatus, 0)
	}
	if response.HTTPStatus == 408 || response.HTTPStatus == 425 || response.HTTPStatus == 429 || response.HTTPStatus >= 500 {
		return retry("http_retryable", response.HTTPStatus, response.RetryAfter)
	}
	return finish("attention", "http_rejected", response.HTTPStatus, 0)
}

func webhookSchemeOK(raw string) bool {
	destination, err := neturl.Parse(raw)
	return err == nil && (destination.Scheme == "http" || destination.Scheme == "https") && destination.Hostname() != ""
}

func (worker *Worker) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			for processed := 0; processed < 20; processed++ {
				worked, err := worker.Process(ctx)
				if err != nil || !worked {
					break
				}
			}
			timer.Reset(3 * time.Second)
		}
	}
}
