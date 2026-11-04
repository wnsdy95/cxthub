package outbound

import (
	"context"
	"time"
)

type NotificationResponse struct {
	HTTPStatus int
	RetryAfter time.Duration
}

type NotificationRequest interface {
	Send(context.Context) (NotificationResponse, error)
}

type NotificationSender interface {
	Prepare(context.Context, NotificationDelivery) (NotificationRequest, error)
}

type NotificationSendError struct {
	Reason string
	Cause  error
}

func (failure *NotificationSendError) Error() string {
	return "notification delivery: " + failure.Reason
}

func (failure *NotificationSendError) Unwrap() error {
	return failure.Cause
}
