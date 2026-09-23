package app

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type invitationEmailConfig struct {
	sender        outbound.InvitationMailer
	from, baseURL string
}

// ConfigureInvitationEmail is startup-only. No credentials enter persisted jobs.
func (s *IdentityService) ConfigureInvitationEmail(sender outbound.InvitationMailer, from, baseURL string) error {
	if sender == nil {
		return fmt.Errorf("invitation email sender is required")
	}
	if _, ok := s.repositories.(outbound.InvitationEmailStore); !ok {
		return fmt.Errorf("invitation email storage unavailable")
	}
	if _, err := mail.ParseAddress(from); err != nil || strings.ContainsAny(from, "\r\n") {
		return fmt.Errorf("invalid RESEND_FROM address")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("CXT_WEB_URL must be a web origin")
	}
	loopback := u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return fmt.Errorf("CXT_WEB_URL requires HTTPS except on loopback")
	}
	s.invitationEmail = &invitationEmailConfig{sender: sender, from: from, baseURL: strings.TrimRight(baseURL, "/")}
	return nil
}

func (s *IdentityService) enqueueInvitationEmail(ctx context.Context, i domain.CollaborationInvite) error {
	if s.invitationEmail == nil {
		return nil
	}
	view, err := s.invitationView(ctx, i)
	if err != nil {
		return err
	}
	link := s.invitationEmail.baseURL + "/invite/" + i.ID
	// Organization names are untrusted; HTML is escaped and the subject is fixed.
	text := fmt.Sprintf("You are invited to %s on CXTHub as %s.\n\nReview invitation: %s\n\nSign in with %s and accept explicitly. This link expires at %s.\nIf you did not expect this invitation, you can ignore it.", view.SpaceName, i.Role, link, i.Email, i.ExpiresAt.UTC().Format(time.RFC3339))
	body := fmt.Sprintf(`<h1>CXTHub invitation</h1><p>You are invited to <strong>%s</strong> as %s.</p><p><a href="%s">Review invitation</a></p><p>Sign in with %s and accept explicitly. Expires: %s.</p><p>If you did not expect this invitation, you can ignore it.</p>`, html.EscapeString(view.SpaceName), html.EscapeString(string(i.Role)), html.EscapeString(link), html.EscapeString(i.Email), i.ExpiresAt.UTC().Format(time.RFC3339))
	return s.repositories.(outbound.InvitationEmailStore).PutInvitationEmail(ctx, domain.InvitationEmail{InvitationID: i.ID, State: "queued", NextAttempt: i.CreatedAt, Message: domain.EmailMessage{From: s.invitationEmail.from, To: []string{i.Email}, Subject: "CXTHub invitation", Text: text, HTML: body}})
}

// claimInvitationEmail checks current invitation authority under the same lock
// as revocation. A later revoke may race with already in-flight email, but can
// never make its link valid again: acceptance always rechecks current authority.
func (s *IdentityService) claimInvitationEmail(ctx context.Context) (domain.InvitationEmail, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.InvitationEmail, error) {
		st := s.repositories.(outbound.InvitationEmailStore)
		j, now, err := st.NextInvitationEmail(ctx)
		if err != nil {
			return j, err
		}
		invites, err := s.invitationStore()
		if err != nil {
			return j, err
		}
		i, err := invites.GetCollaborationInvite(ctx, j.InvitationID)
		if err != nil {
			return j, err
		}
		authorityErr := s.authorizeInvitation(ctx, i.CreatedBy, i.Kind, i.SpaceID, i.Role)
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

func (s *IdentityService) ProcessInvitationEmail(ctx context.Context) (bool, error) {
	if s.invitationEmail == nil {
		return false, nil
	}
	j, err := s.claimInvitationEmail(ctx)
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
	providerID, sendErr := s.invitationEmail.sender.Send(sendCtx, "collaboration-invite/"+j.InvitationID, j.Message)
	cancel()
	err = s.withIdentity(ctx, func(ctx context.Context) error {
		st := s.repositories.(outbound.InvitationEmailStore)
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
func (s *IdentityService) RunInvitationEmailWorker(ctx context.Context) {
	if s.invitationEmail == nil {
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
				worked, err := s.ProcessInvitationEmail(ctx)
				if err != nil || !worked {
					break
				}
			}
			timer.Reset(3 * time.Second)
		}
	}
}
