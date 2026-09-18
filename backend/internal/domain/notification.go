package domain

import "time"

// NotificationJob contains only safe delivery metadata. Destination credentials
// are kept separately by the store and never serialized through the API.
type NotificationJob struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Kind        string    `json:"kind"`
	State       string    `json:"state"`
	Text        string    `json:"text"`
	Attempts    int       `json:"attempts"`
	Version     uint64    `json:"version"`
	Reason      string    `json:"reason,omitempty"`
	HTTPStatus  int       `json:"http_status,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	NextAttempt time.Time `json:"next_attempt"`
	LeaseUntil  time.Time `json:"lease_until"`
}
