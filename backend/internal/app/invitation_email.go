package app

import (
	"context"
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

func (s *IdentityService) claimInvitationEmail(ctx context.Context) (domain.InvitationEmail, error) {
	return s.invitationEmailWorker().claimInvitationEmail(ctx)
}

func (s *IdentityService) ProcessInvitationEmail(ctx context.Context) (bool, error) {
	return s.invitationEmailWorker().ProcessInvitationEmail(ctx)
}
func (s *IdentityService) RunInvitationEmailWorker(ctx context.Context) {
	s.invitationEmailWorker().RunInvitationEmailWorker(ctx)
}
