package domain

import "time"

type AccountAuditEvent struct {
	GrantID   string    `json:"grant_id,omitempty"`
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Action    string    `json:"action"`
	ClientID  string    `json:"client_id"`
	CreatedAt time.Time `json:"created_at"`
}

func ValidateAccountAudit(e AccountAuditEvent) error {
	if ValidateAuditID(e.ID) != nil || ValidateExternalID(e.UserID) != nil || ValidateExternalID(e.ClientID) != nil || e.CreatedAt.IsZero() || (e.Action != "mcp.authorized" && e.Action != "mcp.revoked" && e.Action != "mcp.grant_revoked" && e.Action != "mcp.refresh_reused") {
		return ErrValidation
	}
	if e.GrantID != "" && ValidateExternalID(e.GrantID) != nil {
		return ErrValidation
	}
	if (e.Action == "mcp.grant_revoked" || e.Action == "mcp.refresh_reused") && e.GrantID == "" {
		return ErrValidation
	}
	return nil
}
