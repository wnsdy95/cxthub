package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// All queue mutations and NextInvitationEmail run in the identity transaction.
// Next returns the storage clock so replicas do not schedule leases by host time.
type InvitationEmailStore interface {
	PutInvitationEmail(context.Context, domain.InvitationEmail) error
	GetInvitationEmail(context.Context, string) (domain.InvitationEmail, error)
	NextInvitationEmail(context.Context) (domain.InvitationEmail, time.Time, error)
}
type InvitationMailer interface {
	Send(context.Context, string, domain.EmailMessage) (string, error)
}

// Reason is an allowlisted diagnostic, never provider response text or credentials.
type EmailDeliveryError struct {
	Reason     string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *EmailDeliveryError) Error() string { return e.Reason }
