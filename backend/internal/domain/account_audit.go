package domain

import "time"

type AccountAuditEvent struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Action    string    `json:"action"`
	ClientID  string    `json:"client_id"`
	CreatedAt time.Time `json:"created_at"`
}

func ValidateAccountAudit(e AccountAuditEvent) error {
	if ValidateAuditID(e.ID) != nil || ValidateExternalID(e.UserID) != nil || ValidateExternalID(e.ClientID) != nil || e.CreatedAt.IsZero() || (e.Action != "mcp.authorized" && e.Action != "mcp.revoked") {
		return ErrValidation
	}
	return nil
}
