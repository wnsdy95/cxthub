package domain

import (
	"encoding/base64"
	"encoding/json"
	"time"
)

// The total ordering (timestamp, ID) handles equal timestamps without omissions.
// Organization is bound into the cursor; authorization is still checked on each page.
type AuditCursor struct {
	OrganizationID string    `json:"organization_id"`
	Before         time.Time `json:"before"`
	ID             string    `json:"id"`
}
type OrganizationAuditPage struct {
	Events     []OrganizationAuditEvent `json:"events"`
	NextCursor string                   `json:"next_cursor,omitempty"`
}

func DecodeAuditCursor(org, cursor string) (AuditCursor, error) {
	if cursor == "" {
		return AuditCursor{OrganizationID: org}, nil
	}
	if len(cursor) > 1024 {
		return AuditCursor{}, ErrValidation
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return AuditCursor{}, ErrValidation
	}
	var c AuditCursor
	if json.Unmarshal(b, &c) != nil || c.OrganizationID != org || c.Before.IsZero() || ValidateAuditID(c.ID) != nil {
		return AuditCursor{}, ErrValidation
	}
	return c, nil
}
func EncodeAuditCursor(e OrganizationAuditEvent) string {
	b, _ := json.Marshal(AuditCursor{OrganizationID: e.OrganizationID, Before: e.CreatedAt, ID: e.ID})
	return base64.RawURLEncoding.EncodeToString(b)
}
