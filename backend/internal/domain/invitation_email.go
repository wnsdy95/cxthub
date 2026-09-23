package domain

import "time"

// InvitationEmail freezes the payload before delivery. Retrying the same ID
// must never change the sender, recipient, text or link (provider idempotency).
type InvitationEmail struct {
	InvitationID string       `json:"invitation_id"`
	Message      EmailMessage `json:"message"`
	State        string       `json:"state"`
	Reason       string       `json:"reason,omitempty"`
	ProviderID   string       `json:"provider_id,omitempty"`
	Attempts     int          `json:"attempts"`
	FirstAttempt time.Time    `json:"first_attempt,omitempty"`
	NextAttempt  time.Time    `json:"next_attempt"`
}
type EmailMessage struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
	Text    string   `json:"text"`
}

func (j InvitationEmail) Ready(now time.Time) bool {
	return (j.State == "queued" || j.State == "retrying" || j.State == "sending") && !j.NextAttempt.After(now)
}
func ValidateInvitationEmail(j InvitationEmail) error {
	if ValidateCollaborationInviteID(j.InvitationID) != nil || j.Attempts < 0 || j.NextAttempt.IsZero() || len(j.Message.To) != 1 || j.Message.From == "" || j.Message.Text == "" {
		return ErrValidation
	}
	if email, err := NormalizeInvitationEmail(j.Message.To[0]); err != nil || email != j.Message.To[0] {
		return ErrValidation
	}
	// A missing first-attempt timestamp must never reopen the deduplication window.
	if (j.Attempts == 0) != j.FirstAttempt.IsZero() {
		return ErrValidation
	}
	if j.State == "queued" && j.Attempts != 0 {
		return ErrValidation
	}
	if (j.State == "sending" || j.State == "retrying" || j.State == "accepted" || j.State == "attention") && j.Attempts == 0 {
		return ErrValidation
	}
	if j.State == "accepted" && j.ProviderID == "" {
		return ErrValidation
	}
	switch j.State {
	case "queued", "sending", "retrying", "accepted", "attention", "cancelled":
	default:
		return ErrValidation
	}
	return nil
}
