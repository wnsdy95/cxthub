package app

import (
	"context"
	"errors"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// claimInvitationEmail checks current invitation authority under the same lock
// as revocation. A later revoke may race with already in-flight email, but can
// never make its link valid again: acceptance always rechecks current authority.
func (worker *InvitationEmailWorker) claimInvitationEmail(ctx context.Context) (domain.InvitationEmail, error) {
	return identityResultWithTransactions(ctx, worker.transactions, func(ctx context.Context) (domain.InvitationEmail, error) {
		st := worker.queue
		j, now, err := st.NextInvitationEmail(ctx)
		if err != nil {
			return j, err
		}
		if worker.invitations == nil {
			return j, domain.ErrForbidden
		}
		i, err := worker.invitations.GetCollaborationInvite(ctx, j.InvitationID)
		if err != nil {
			return j, err
		}
		authorityErr := worker.authority.authorizeInvitation(ctx, i.CreatedBy, i.Kind, i.SpaceID, i.Role)
		if authorityErr != nil && !errors.Is(authorityErr, domain.ErrForbidden) {
			return j, authorityErr
		}
		switch {
		case i.Status != "pending" || !now.Before(i.ExpiresAt):
			j.State, j.Reason = "cancelled", "invitation_inactive"
		case authorityErr != nil:
			j.State, j.Reason = "cancelled", "issuer_not_authorized"
		case !j.FirstAttempt.IsZero() && !now.Before(j.FirstAttempt.Add(23*time.Hour)):
			j.State, j.Reason = "attention", "retry_window_expired"
		case j.Attempts >= 12:
			j.State, j.Reason = "attention", "attempts_exhausted"
		default:
			j.State, j.Reason = "sending", ""
			j.Attempts++
			if j.FirstAttempt.IsZero() {
				j.FirstAttempt = now
			}
			j.NextAttempt = now.Add(2 * time.Minute)
		}
		if err := st.PutInvitationEmail(ctx, j); err != nil {
			return j, err
		}
		return j, nil
	})
}

func (worker *InvitationEmailWorker) ProcessInvitationEmail(ctx context.Context) (bool, error) {
	if worker.sender == nil {
		return false, nil
	}
	j, err := worker.claimInvitationEmail(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if j.State != "sending" {
		return true, nil
	}
	// Never hold the identity transaction across the provider request.
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	providerID, sendErr := worker.sender.Send(sendCtx, "collaboration-invite/"+j.InvitationID, j.Message)
	cancel()
	err = withIdentityTransactions(ctx, worker.transactions, func(ctx context.Context) error {
		st := worker.queue
		current, err := st.GetInvitationEmail(ctx, j.InvitationID)
		if err != nil {
			return err
		}
		if current.State != "sending" || current.Attempts != j.Attempts {
			return domain.ErrConflict
		}
		current.State, current.Reason, current.ProviderID = "accepted", "", providerID
		if sendErr != nil {
			failure := &outbound.EmailDeliveryError{Reason: "transport_failed", Retryable: true}
			var classified *outbound.EmailDeliveryError
			if errors.As(sendErr, &classified) {
				failure = classified
			}
			current.State, current.Reason, current.ProviderID = "attention", failure.Reason, ""
			if failure.Retryable && j.Attempts < 12 {
				delay := min(time.Duration(1<<min(j.Attempts, 10))*30*time.Second, time.Hour)
				if failure.RetryAfter > delay {
					delay = failure.RetryAfter
				}
				// The lease's start is the storage clock. Add the bounded request time so
				// Retry-After is respected even when wall clocks differ between replicas.
				current.NextAttempt = j.NextAttempt.Add(-2*time.Minute + 10*time.Second + delay)
				if current.NextAttempt.Before(j.FirstAttempt.Add(23 * time.Hour)) {
					current.State = "retrying"
				} else {
					current.Reason = "retry_window_expired"
				}
			}
		}
		return st.PutInvitationEmail(ctx, current)
	})
	return true, err
}

func (worker *InvitationEmailWorker) RunInvitationEmailWorker(ctx context.Context) {
	if worker.sender == nil {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			for n := 0; n < 10; n++ {
				worked, err := worker.ProcessInvitationEmail(ctx)
				if err != nil || !worked {
					break
				}
			}
			timer.Reset(3 * time.Second)
		}
	}
}
