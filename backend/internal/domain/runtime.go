package domain

import "time"

// DevicePairing holds only the hash of the CLI's receipt secret.
type DevicePairing struct {
	Code      string    `json:"code"`
	PollHash  string    `json:"poll_hash"`
	UserID    string    `json:"user_id,omitempty"`
	Label     string    `json:"label,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}
